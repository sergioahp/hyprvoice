package transcriber

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/leonardotrapani/hyprvoice/internal/provider"
	"github.com/sashabaranov/go-openai"
)

// Prompt ceilings, both measured in characters.
//
// gpt-transcribe rejects prompts over 65,536 characters ("prompt must be at
// most 65536 characters"). The limit is not in OpenAI's docs; it was found by
// probing the endpoint. That model bills per audio minute only, so a large
// prompt costs nothing extra.
//
// gpt-4o-transcribe has no character limit but shares a 16,000-token context
// between audio and prompt. Reserve ~4,096 tokens (~25%), leaving ~12k for
// audio. Terminal text (code, commands, output) encodes at roughly 3 chars per
// token with o200k_base, so 4096 x 3 = 12,288 chars is the conservative
// ceiling. Note this model bills those prompt tokens on every request.
const (
	maxContextFieldsPromptChars = 65536
	maxLegacyPromptChars        = 12288
)

// OpenAIAdapterConfig configures an OpenAI-compatible transcription adapter.
type OpenAIAdapterConfig struct {
	Endpoint     *provider.EndpointConfig
	APIKey       string
	Model        string
	ProviderName string // used for logging and language format conversion

	Language  string   // provider language code; mutually exclusive with Languages
	Languages []string // expected input languages, ContextFields models only
	Keywords  []string // literal spelling hints

	ContextPrompt string // terminal scrollback passed as transcription prompt

	// ContextFields mirrors provider.Model.SupportsContextFields: the model
	// accepts `keywords` and `languages`, which the vendored SDK cannot send.
	ContextFields bool
}

// OpenAIAdapter implements BatchAdapter for any OpenAI-compatible API
// Works with OpenAI, Groq, Mistral, and any other OpenAI-compatible endpoint
type OpenAIAdapter struct {
	client        *openai.Client
	httpClient    *http.Client
	apiKey        string
	transcribeURL string
	model         string
	language      string
	languages     []string
	keywords      []string
	providerName  string
	contextPrompt string
	contextFields bool
}

// NewOpenAIAdapter creates an adapter for OpenAI-compatible transcription APIs
func NewOpenAIAdapter(cfg OpenAIAdapterConfig) *OpenAIAdapter {
	var client *openai.Client

	transcribeURL := "https://api.openai.com/v1/audio/transcriptions"
	if cfg.Endpoint != nil && cfg.Endpoint.BaseURL != "" {
		// use custom endpoint
		clientConfig := openai.DefaultConfig(cfg.APIKey)
		clientConfig.BaseURL = cfg.Endpoint.BaseURL + "/v1"
		client = openai.NewClientWithConfig(clientConfig)
		transcribeURL = cfg.Endpoint.BaseURL + cfg.Endpoint.Path
	} else {
		// default to OpenAI
		client = openai.NewClient(cfg.APIKey)
	}

	return &OpenAIAdapter{
		client:        client,
		httpClient:    &http.Client{},
		apiKey:        cfg.APIKey,
		transcribeURL: transcribeURL,
		model:         cfg.Model,
		language:      cfg.Language,
		languages:     cfg.Languages,
		keywords:      cfg.Keywords,
		providerName:  cfg.ProviderName,
		contextPrompt: cfg.ContextPrompt,
		contextFields: cfg.ContextFields,
	}
}

func (a *OpenAIAdapter) Transcribe(ctx context.Context, audioData []byte) (string, error) {
	if len(audioData) == 0 {
		return "", nil
	}

	// Convert raw PCM to WAV format
	wavData, err := convertToWAV(audioData)
	if err != nil {
		return "", fmt.Errorf("convert to WAV: %w", err)
	}

	start := time.Now()
	var text string
	if a.contextFields {
		text, err = a.transcribeWithContextFields(ctx, wavData)
	} else {
		text, err = a.transcribeViaSDK(ctx, wavData)
	}
	duration := time.Since(start)

	if err != nil {
		log.Printf("%s-adapter: API call failed after %v: %v", a.providerName, duration, err)
		return "", fmt.Errorf("%s transcription: %w", a.providerName, err)
	}

	log.Printf("%s-adapter: transcribed %d bytes in %v: %q", a.providerName, len(audioData), duration, text)
	return text, nil
}

func (a *OpenAIAdapter) transcribeViaSDK(ctx context.Context, wavData []byte) (string, error) {
	req := openai.AudioRequest{
		Model:    a.model,
		Reader:   bytes.NewReader(wavData),
		FilePath: "audio.wav",
		Language: a.language,
		Prompt:   a.prompt(maxLegacyPromptChars),
	}

	resp, err := a.client.CreateTranscription(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// transcribeWithContextFields builds the multipart request by hand because
// go-openai's AudioRequest has no Keywords or Languages field at any version;
// its form writer only emits file, model, prompt, response_format, temperature,
// language and timestamp_granularities[].
func (a *OpenAIAdapter) transcribeWithContextFields(ctx context.Context, wavData []byte) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	fw, err := w.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(wavData); err != nil {
		return "", fmt.Errorf("write audio: %w", err)
	}

	fields := [][2]string{{"model", a.model}}

	// `languages` replaces the singular `language` on these models; sending
	// both is rejected with 400 invalid_value.
	if len(a.languages) > 0 {
		for _, lang := range a.languages {
			fields = append(fields, [2]string{"languages", lang})
		}
	} else if a.language != "" {
		fields = append(fields, [2]string{"language", a.language})
	}

	for _, kw := range sanitizeKeywords(a.keywords) {
		fields = append(fields, [2]string{"keywords", kw})
	}

	if p := a.prompt(maxContextFieldsPromptChars); p != "" {
		fields = append(fields, [2]string{"prompt", p})
	}

	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return "", fmt.Errorf("writing %s: %w", f[0], err)
		}
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.transcribeURL, &body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Text      string `json:"text"`
		Languages []struct {
			Code string `json:"code"`
		} `json:"languages"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(parsed.Languages) > 0 {
		codes := make([]string, len(parsed.Languages))
		for i, l := range parsed.Languages {
			codes[i] = l.Code
		}
		log.Printf("%s-adapter: detected languages: %s", a.providerName, strings.Join(codes, ", "))
	}

	return parsed.Text, nil
}

// prompt returns the conditioning prompt capped to maxChars.
// contextPrompt (scrollback) takes precedence over keyword hints.
// Keep the tail so the most recent content (most relevant vocabulary) is preserved.
// Models with dedicated context fields send keywords separately, so they never
// fold them into the prompt.
func (a *OpenAIAdapter) prompt(maxChars int) string {
	switch {
	case a.contextPrompt != "":
		return tailChars(a.contextPrompt, maxChars)
	case len(a.keywords) > 0 && !a.contextFields:
		return strings.Join(a.keywords, ", ")
	}
	return ""
}

// tailChars keeps the last n characters of s. The API counts characters rather
// than bytes, and slicing bytes would risk cutting a multi-byte rune in half.
func tailChars(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}

// sanitizeKeywords drops the characters the API rejects (<, >, CR, LF) rather
// than letting one malformed hint fail the whole request.
func sanitizeKeywords(keywords []string) []string {
	if len(keywords) == 0 {
		return nil
	}
	replacer := strings.NewReplacer("<", "", ">", "", "\r", " ", "\n", " ")
	out := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		if cleaned := strings.TrimSpace(replacer.Replace(kw)); cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return out
}

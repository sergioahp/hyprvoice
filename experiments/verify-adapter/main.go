// Diagnostic: exercise the hand-built gpt-transcribe multipart request through
// the real adapter, so a malformed form field surfaces here instead of the
// first time the user speaks. Retained with its PCM fixture for later probes.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/leonardotrapani/hyprvoice/internal/provider"
	"github.com/leonardotrapani/hyprvoice/internal/transcriber"
)

func main() {
	audioPath := "experiments/verify-adapter/audio.pcm"
	if len(os.Args) > 1 {
		audioPath = os.Args[1]
	}
	pcm, err := os.ReadFile(audioPath)
	if err != nil {
		panic(err)
	}

	model, err := provider.GetModel("openai", "gpt-transcribe")
	if err != nil {
		panic(err)
	}
	fmt.Printf("model=%s SupportsContextFields=%v\n", model.ID, model.SupportsContextFields)

	adapter := transcriber.NewOpenAIAdapter(transcriber.OpenAIAdapterConfig{
		Endpoint:      model.Endpoint,
		APIKey:        os.Getenv("OPENAI_API_KEY"),
		Model:         model.ID,
		ProviderName:  "openai",
		Languages:     []string{"es", "en"},
		Keywords:      []string{"Hyprlock", "Hyprland", "bad<keyword>with\nnewline"},
		ContextPrompt: "user@nixd ~/code/go/hyprvoice $ hyprvoice serve\nEditing hm-module.nix for Hyprlock.\n",
		ContextFields: model.SupportsContextFields,
	})

	text, err := adapter.Transcribe(context.Background(), pcm)
	if err != nil {
		fmt.Println("FAILED:", err)
		os.Exit(1)
	}
	fmt.Printf("OK: %q\n", text)
}

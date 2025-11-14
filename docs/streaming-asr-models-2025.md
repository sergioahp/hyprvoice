# Streaming ASR Models & Revision Techniques (2025)

## Overview

This document covers modern speech-to-text models with native streaming support and techniques for transcript revision/correction.

---

## Table of Contents

1. [Whisper's Streaming Limitations](#whispers-streaming-limitations)
2. [Modern Native Streaming Models](#modern-native-streaming-models)
3. [Google's Two-Pass Deliberation](#googles-two-pass-deliberation)
4. [Revision Support in 2025](#revision-support-in-2025)
5. [DIY Approaches](#diy-approaches)

---

## Whisper's Streaming Limitations

### Why Whisper Can't Stream Natively

Whisper is fundamentally **incompatible** with true streaming:

- **Architecture**: Encoder-decoder transformer
- **Input requirement**: Expects 30-second audio chunks (hardcoded)
- **Processing**: Requires full context windows
- **Decoding**: Uses beam search (not incremental)
- **Multiple passes**: Runs several inference iterations for accuracy

### All Whisper "Streaming" Uses Chunking Tricks

| Implementation | Method | Real Streaming? |
|---------------|--------|-----------------|
| OpenAI API | ❌ No streaming | ❌ No |
| Groq API | ❌ Batch only | ❌ No |
| whisper.cpp | Chunking + VAD | ❌ No (simulated) |
| WhisperLive | Chunking + buffering | ❌ No (simulated) |
| whisper_streaming | Sliding window | ❌ No (simulated) |

**How they fake it:**
```python
# Pseudo-code for whisper.cpp streaming
while recording:
    buffer_audio(0.5_seconds)

    if buffer >= 30_seconds:
        chunks = split_into_overlapping_chunks(buffer)
        for chunk in chunks:
            transcription = whisper.process(chunk)  # Full 30s
            output(transcription)
```

### OpenAI's Real Streaming: Realtime API

OpenAI created a **completely different product** for true streaming:

- **Model**: GPT-4o (not Whisper)
- **Architecture**: Speech-to-speech (bypasses traditional ASR)
- **Latency**: ~320ms average
- **Bidirectional**: Can interrupt mid-sentence
- **Price**: $0.06/min input + $0.24/min output
- **Use case**: Conversational AI (not pure transcription)

---

## Modern Native Streaming Models

### 1. Deepgram Nova-3 🏆 (Commercial - Best Overall)

**Released**: February 2025

```yaml
Architecture: Custom streaming RNN-T
Streaming: TRUE native real-time
Latency: <500ms
WER Improvement: 54.3% over Nova-2
Languages: 50+ languages
Speed: Faster than real-time
Price: $0.0043-0.0137/min (tiered)
```

**Key Features:**
- Built from scratch for streaming (not adapted)
- Handles noisy environments exceptionally well
- Multi-stage curriculum learning
- Embedding encoder + Transformer-XL backbone
- No revision/two-pass (single-pass excellence)

**Best for**: Production voice agents, enterprise applications

---

### 2. AssemblyAI Universal-Streaming ⚡ (Commercial - Best Latency)

**Released**: 2024-2025

```yaml
Architecture: Conformer-based streaming
Streaming: TRUE native real-time
Latency: 300ms (P50) - fastest in class
Model: Universal-2 (RNN-T decoder)
Languages: Multiple (English-focused)
Price: $0.15/hour ($0.0025/min)
Uptime SLA: 99.95%
```

**Key Features:**
- **Immutable transcripts** (never changes once emitted)
- Designed to avoid revision cycles
- No "word flickering" in UI
- WebSocket streaming protocol
- Emits finals immediately (subword-level)

**Philosophy**: "Get it right the first time" - no need for revision

**Best for**: Voice agents requiring consistent, reliable transcripts

---

### 3. Kyutai Moshi / STT-2.6B 🔓 (Open Source - Most Innovative)

**Released**: 2024

```yaml
Architecture: Multi-stream transformer + Mimi codec
Streaming: TRUE full-duplex streaming
Latency: 160ms theoretical, 200ms practical
WER: 5.63% (Canary Qwen 2.5B variant)
Languages: English, French
Speed: RTFx 418 (Canary)
License: Apache 2.0
```

**Key Features:**
- **Full-duplex** conversational AI
- Mimi audio codec (80ms latency encoding)
- Multi-stream modeling (audio + text concurrently)
- Supports interruptions, backchanneling
- Single model for ASR + TTS

**Architecture Innovation:**
- Decoder-only transformer
- Inner monologue method (delayed text tokens)
- Can switch ASR/TTS with single hyperparameter
- 24kHz audio → 12.5Hz representation @ 1.1kbps

**Best for**: Research, conversational AI experiments

---

### 4. NVIDIA Parakeet-TDT-0.6B 🚀 (Open Source - Best Speed)

**Released**: 2025 (v3 multilingual)

```yaml
Architecture: Token-and-Duration Transducer (TDT)
Streaming: TRUE native (buffered streaming)
Latency: Ultra-low (1 hour in 1 second)
WER: Top of HuggingFace Open ASR leaderboard
Languages: 25 European languages (v3)
Speed: RTFx 3380
License: CC-BY-4.0
```

**Key Features:**
- **TDT innovation**: Predicts token durations
- Skips frames intelligently (64% faster than RNNT)
- Designed for edge devices
- Buffered streaming via NeMo

**Architecture Advantage:**
- Extends RNN-Transducer
- Jointly predicts tokens + durations
- Frame skipping based on predictions
- Much faster than frame-by-frame

**Best for**: Edge deployment, low-latency applications

---

### 5. Speechmatics Real-Time ASR 💼 (Commercial - Enterprise)

```yaml
Architecture: Attention-based with streaming optimization
Streaming: TRUE native
Languages: 30+ languages
Features: On-premises, domain adaptation
Price: Custom enterprise pricing
```

**Best for**: Enterprise with data privacy requirements

---

## Architecture Types Comparison

### Type 1: RNN-Transducer (True Streaming)

**Examples**: Parakeet-TDT, traditional RNN-T

**How it works:**
- Processes frames incrementally
- Emits tokens as it goes
- Native streaming by design
- Very low latency

### Type 2: Streaming Conformer/Transformer

**Examples**: Deepgram Nova, AssemblyAI

**How it works:**
- Transformer with causal/streaming modifications
- Limited lookahead
- Requires architectural tricks
- Low latency (<500ms)

### Type 3: Multi-Stream Models

**Examples**: Kyutai Moshi

**How it works:**
- Multiple token streams processed concurrently
- Audio + text streams
- Full-duplex capability
- Ultra-low latency (160-200ms)

### Type 4: Speech-to-Speech

**Examples**: OpenAI Realtime API

**How it works:**
- Bypasses traditional ASR entirely
- Direct audio understanding
- Bidirectional streaming
- ~320ms latency

---

## Google's Two-Pass Deliberation

### The Breakthrough Paper

**"Deliberation Model Based Two-Pass End-to-End Speech Recognition"**
Google Research, ICASSP 2020

### How It Works

```
┌─────────────────────────────────────────┐
│  PASS 1: Streaming RNN-T (fast)        │
│  "the whether is grate today"          │ ← Mistakes!
└─────────────┬───────────────────────────┘
              │
              ▼
┌─────────────────────────────────────────┐
│  PASS 2: Deliberation Network           │
│  • Looks at acoustics AGAIN             │
│  • Looks at Pass 1 hypothesis           │
│  • Uses bidirectional encoder           │
│  • Rescores with full context           │
│  "the weather is great today"           │ ← Fixed!
└─────────────────────────────────────────┘
```

### Key Innovation

**Dual Attention:**
- Attends to **both** acoustic features AND first-pass text
- Previous approaches used only one or the other
- Bidirectional encoder extracts context from first-pass

### Performance Results

- **25% WER reduction** vs pure RNN-T
- **12% improvement** vs LAS rescoring
- **23% improvement** on proper nouns
- Maintains reasonable latency

### Architecture Details

1. **First Pass**: Streaming RNN-T (low latency, some errors)
2. **Deliberation Network**:
   - Bidirectional encoder on first-pass hypothesis
   - Attends to both acoustics + hypothesis
   - Non-streaming (can see full context)
   - Rescores and corrects mistakes

---

## Revision Support in 2025

### Models With Revision Capabilities

| Model | Revision Support | Method |
|-------|------------------|--------|
| **Deepgram Nova-3** | ❌ No | Single-pass accuracy (no need) |
| **AssemblyAI Universal-Streaming** | ❌ No (intentional) | Immutable by design |
| **Kyutai Moshi** | ⚠️ Research | Multi-stream (not two-pass) |
| **Parakeet-TDT** | ❌ No | Single-pass TDT |
| **Bloomberg Whisper-U2** | ✅ YES | Two-pass U2 framework |
| **Google Internal** | ✅ YES | Deliberation (not public) |

### Why Production Models Avoid Revision

#### AssemblyAI's Reasoning

**Problem with mutable transcripts:**
```
User says: "Call John"
System sees: "Call John" → starts dialing
500ms later, revision: "Call Jonathan" → Too late!
```

**Solution**: Immutable transcripts
- Emit finals immediately
- Never change once emitted
- Trade slightly lower max accuracy for consistency
- No "word flickering" in UI

#### Deepgram's Approach

- Invest heavily in single-pass accuracy
- Multi-stage curriculum learning
- Advanced training (synthetic + real data)
- Goal: Get it right the first time

---

### Bloomberg's Whisper-U2 (Interspeech 2025)

**"Adapting Whisper for Streaming via Two-Pass Decoding"**

```yaml
Architecture: Unified Two-Pass (U2)
Pass 1: Lightweight CTC decoder (streaming)
Pass 2: Original Whisper attention decoder (rescoring)
Status: Research paper
```

**How It Works:**

1. **CTC Decoder** (Pass 1):
   - Causally-masked
   - Emits draft transcripts as audio arrives
   - Low latency

2. **Whisper Decoder** (Pass 2):
   - Original attention mechanism
   - Rescores CTC drafts
   - High quality

3. **Hybrid Tokenizer**:
   - Shrinks CTC token set
   - Enables data-efficient fine-tuning

**Innovation**: Makes Whisper streaming-capable while maintaining accuracy

---

## DIY Approaches

### 1. Two-Pass with Whisper Prompt

```python
# PASS 1: Fast streaming (chunked)
def pass1_streaming(audio):
    chunks = chunk_audio(audio, chunk_size=5)
    hypotheses = []

    for chunk in chunks:
        result = groq.transcribe(
            chunk,
            model="whisper-large-v3-turbo"  # Fast!
        )
        hypotheses.append(result)

    return " ".join(hypotheses)

# PASS 2: Deliberation (full audio)
def pass2_rescore(full_audio, pass1_text):
    result = groq.transcribe(
        full_audio,
        model="whisper-large-v3",  # More accurate
        prompt=pass1_text,  # Context from pass1!
        temperature=0.0
    )

    return result  # Refined output
```

**Benefits:**
- Pass 1: Low latency (chunk-by-chunk)
- Pass 2: High accuracy (full context + prompt)
- `prompt` parameter helps Whisper understand context

**Limitations:**
- Not true deliberation (Whisper doesn't attend to hypothesis)
- Still uses chunking tricks
- Higher cost (two API calls)

---

### 2. LLM Post-Correction

Modern approach: Use LLMs to fix ASR errors

```python
# Get ASR output
asr_text = groq_whisper.transcribe(audio)

# Fix with LLM
corrected = llm.complete(f"""
Fix any speech recognition errors in this text:
"{asr_text}"

Rules:
- Fix homophone errors (there/their/they're)
- Fix grammar
- Add punctuation
- Don't change meaning

Output only the corrected text.
""")
```

**Recent Research:**
- "ASR Error Correction using Large Language Models" (arXiv 2024)
- Shows LLMs can fix ASR errors effectively
- Especially good for domain-specific terminology

**Advantages:**
- Can fix contextual errors Whisper can't
- Works with any ASR system
- Flexible (can add custom rules)

**Disadvantages:**
- Additional latency
- LLM API cost
- May introduce new errors

---

### 3. Hyprvoice Implementation Example

For the toggle workflow (not streaming):

```python
# Two-pass approach for better accuracy
audio = record_toggle()

# Optional: Pass 1 - Fast preview
preview = groq.transcribe(
    audio,
    model="whisper-large-v3-turbo"
)
show_notification(f"Preview: {preview}")

# Pass 2: Final accurate version
final = groq.transcribe(
    audio,
    model="whisper-large-v3",
    prompt=preview  # Use preview as context
)
inject_text(final)
```

**Alternative: Single pass with LLM correction**

```python
# ASR transcription
audio = record_toggle()
transcript = groq.transcribe(audio, model="whisper-large-v3-turbo")

# LLM correction
corrected = llm.fix_errors(transcript)
inject_text(corrected)
```

---

## Academic Models with Lattice/N-Best Rescoring

Many research frameworks support advanced rescoring:

### WeNet
- Supports lattice generation
- External LM rescoring
- Contextual biasing

### ESPnet
- N-best hypothesis generation
- LM fusion
- Joint CTC/Attention rescoring

### NeMo (NVIDIA)
- External LM rescoring
- Contextual biasing
- Confidence estimation

---

## Recommendations by Use Case

### For Production Voice Agents
1. **AssemblyAI Universal-Streaming** - Best latency, immutable
2. **Deepgram Nova-3** - Best accuracy, multilingual

### For Research/Experimentation
1. **Kyutai Moshi** - Full-duplex, open source
2. **Parakeet-TDT** - Fast, edge-capable
3. **Bloomberg Whisper-U2** - Two-pass research

### For Hyprvoice (Toggle Workflow)
1. **Groq Whisper-large-v3-turbo** - Perfect for batch (current)
2. **DIY two-pass** - For better accuracy when needed
3. **Whisper + LLM** - For domain-specific corrections

---

## Key Takeaways

1. **Whisper cannot stream natively** - it's architectural
2. **All Whisper "streaming" uses chunking** - it's simulated
3. **Modern streaming models** use RNN-T, streaming Conformer, or multi-stream architectures
4. **Revision/deliberation is rare** in production (2025) - single-pass models got so good
5. **AssemblyAI chose immutability** over revision (UX reasons)
6. **You can DIY two-pass** with Whisper prompt or LLM correction
7. **Google's deliberation** is research/internal only

---

## References

### Papers
- "Deliberation Model Based Two-Pass End-to-End Speech Recognition" (Google, ICASSP 2020)
- "Adapting Whisper for Streaming via Two-Pass Decoding" (Bloomberg, Interspeech 2025)
- "Less Is More: Improved RNN-T Decoding" (Google Research, 2021)
- "Moshi: A Speech-Text Foundation Model for Real-Time Dialogue" (Kyutai, 2024)

### Models & APIs
- [Deepgram Nova-3](https://deepgram.com/learn/introducing-nova-3-speech-to-text-api)
- [AssemblyAI Universal-Streaming](https://www.assemblyai.com/blog/introducing-universal-streaming)
- [Kyutai Moshi](https://github.com/kyutai-labs/moshi)
- [NVIDIA Parakeet-TDT](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3)
- [OpenAI Realtime API](https://openai.com/index/introducing-the-realtime-api/)

---

*Last Updated: November 2025*

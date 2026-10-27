#!/usr/bin/env python3
"""Test gpt-4o-transcribe with unprocessed terminal context as a vocabulary prompt.

Workflow:
  1. Generate TTS audio simulating the user giving technical instructions with jargon.
  2. Transcribe via gpt-4o-transcribe, passing the context file as the prompt.
  3. Print the plain transcription.

Usage:
    OPENAI_API_KEY=sk-... python3 test_transcribe_context.py [context_file] [audio.wav]

Defaults: claude-computer-setup-summary.txt, testdata/tts_jargon.wav (generated if absent).

Run with: nix develop .#python --command python3 test_transcribe_context.py
"""

import os
import sys
from pathlib import Path

from openai import OpenAI

CONTEXT_FILE = "claude-computer-setup-summary.txt"
AUDIO_OUT = "testdata/tts_jargon.wav"

TTS_TEXT = (
    "I'm setting up hyprvoice on NixOS using a nix flake and Home Manager. "
    "I edit the config in neovim and use hyprlock for screen locking under hyprland. "
    "The test script reads a WAV file, resamples it to twenty-four kilohertz, "
    "then streams it over a websocket. We need to send a session update to enable "
    "audio input transcription for gpt-realtime-whisper, then commit the audio buffer manually. "
    "Can you check the hyprwhisper integration and make sure dunst replaces the same notification ID?"
)


def generate_tts(client: OpenAI, path: str) -> None:
    print(f"generating TTS -> {path}")
    resp = client.audio.speech.create(
        model="tts-1", voice="alloy", input=TTS_TEXT, response_format="wav"
    )
    Path(path).write_bytes(resp.content)


def transcribe(client: OpenAI, context: str, audio_path: str) -> str:
    print("transcribing with gpt-4o-transcribe...")
    with open(audio_path, "rb") as f:
        resp = client.audio.transcriptions.create(
            model="gpt-4o-transcribe",
            file=f,
            prompt=context,
        )
    return resp.text


def main():
    api_key = os.environ.get("OPENAI_API_KEY", "")
    if not api_key:
        sys.exit("set OPENAI_API_KEY")

    context_path = sys.argv[1] if len(sys.argv) > 1 else CONTEXT_FILE
    audio_path = sys.argv[2] if len(sys.argv) > 2 else AUDIO_OUT

    context = Path(context_path).read_text()
    print(f"context: {context_path} ({len(context)} chars, {len(context.splitlines())} lines)")

    client = OpenAI()

    if not Path(audio_path).exists():
        generate_tts(client, audio_path)
    else:
        print(f"reusing existing audio: {audio_path}")

    transcript = transcribe(client, context, audio_path)
    print(f"\nTRANSCRIPT:\n{transcript}")


if __name__ == "__main__":
    main()

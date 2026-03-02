package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dir2mcp/internal/dirstral/voice"
)

func TestVoiceRunFailsFastWithoutAudioPrereqs(t *testing.T) {
	binDir := t.TempDir()
	ffmpegPath := filepath.Join(binDir, "ffmpeg")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ELEVENLABS_API_KEY", "")

	err := voice.Run(context.Background(), voice.Options{Mute: true})
	if err == nil {
		t.Fatal("expected voice run to fail without local audio prerequisites")
	}

	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "elevenlabs_api_key is required") {
		t.Fatalf("unexpected voice preflight error: %v", err)
	}
}

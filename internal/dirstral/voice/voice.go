package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"dir2mcp/internal/dirstral/mcp"
	"dir2mcp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

type Options struct {
	MCPURL         string
	Transport      string
	Model          string
	Voice          string
	Device         string
	Mute           bool
	Verbose        bool
	BaseURL        string
	TranscriptOnly bool
}

func Run(ctx context.Context, opts Options) error {
	if err := preflight(opts); err != nil {
		return err
	}

	if strings.TrimSpace(opts.Transport) == "" {
		opts.Transport = protocol.DefaultTransport
	}
	if strings.TrimSpace(opts.Model) == "" {
		opts.Model = protocol.DefaultModel
	}

	client := mcp.NewWithTransport(opts.MCPURL, opts.Transport, opts.Verbose)
	defer func() {
		_ = client.Close()
	}()
	if err := client.Initialize(ctx); err != nil {
		return err
	}

	p := tea.NewProgram(initialModel(ctx, client, opts), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return err
	}
	return nil
}

// recordAudio writes a temporary WAV file and returns its path.
// Callers are responsible for removing the returned file.
func recordAudio(ctx context.Context, device string) (string, error) {
	out := filepath.Join(os.TempDir(), fmt.Sprintf("dirstral-%d.wav", time.Now().UnixNano()))
	var attempts [][]string
	switch runtime.GOOS {
	case "darwin":
		input, err := resolveMacInputDevice(device)
		if err != nil {
			return "", err
		}
		attempts = append(attempts, []string{"-y", "-f", "avfoundation", "-i", input, "-t", "6", "-ac", "1", "-ar", "16000", out})
	case "linux":
		if strings.TrimSpace(device) != "" {
			attempts = append(attempts, []string{"-y", "-f", "alsa", "-i", strings.TrimSpace(device), "-t", "6", "-ac", "1", "-ar", "16000", out})
		} else {
			attempts = append(attempts,
				[]string{"-y", "-f", "alsa", "-i", "default", "-t", "6", "-ac", "1", "-ar", "16000", out},
				[]string{"-y", "-f", "pulse", "-i", "default", "-t", "6", "-ac", "1", "-ar", "16000", out},
			)
		}
	default:
		return "", fmt.Errorf("unsupported OS for quick mic recording: %s", runtime.GOOS)
	}

	var lastErr error
	for _, args := range attempts {
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			return out, nil
		} else {
			lastErr = err
		}
	}
	return "", fmt.Errorf("failed to capture audio, last error: %w", lastErr)
}

func transcribeElevenLabs(ctx context.Context, baseURL, audioPath string) (string, error) {
	apiKey := strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY"))
	if apiKey == "" {
		return "", fmt.Errorf("ELEVENLABS_API_KEY is required")
	}
	buf := &bytes.Buffer{}
	writer := multipart.NewWriter(buf)
	if err := writer.WriteField("model_id", "scribe_v1"); err != nil {
		return "", err
	}
	fw, err := writer.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", err
	}
	f, err := os.Open(audioPath)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
	}()
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/speech-to-text", buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("xi-api-key", apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read stt response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("stt http %d: %s", resp.StatusCode, string(body))
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	text, _ := parsed["text"].(string)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("empty transcript")
	}
	return text, nil
}

func playAudio(ctx context.Context, path string) error {
	var cmd *exec.Cmd
	if _, err := exec.LookPath("afplay"); err == nil {
		cmd = exec.CommandContext(ctx, "afplay", path)
	} else if _, err := exec.LookPath("ffplay"); err == nil {
		cmd = exec.CommandContext(ctx, "ffplay", "-nodisp", "-autoexit", path)
	} else {
		return fmt.Errorf("no playback binary found (afplay or ffplay)")
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func preflight(opts Options) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg is required for Voice mic recording")
	}
	if strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY")) == "" {
		return fmt.Errorf("ELEVENLABS_API_KEY is required")
	}
	if opts.Mute || opts.TranscriptOnly {
		return nil
	}
	if _, err := exec.LookPath("afplay"); err == nil {
		return nil
	}
	if _, err := exec.LookPath("ffplay"); err == nil {
		return nil
	}
	return fmt.Errorf("audio playback requires afplay (macOS) or ffplay")
}

func resolveMacInputDevice(device string) (string, error) {
	trimmed := strings.TrimSpace(device)
	if trimmed == "" {
		return ":0", nil
	}
	if _, err := strconv.Atoi(trimmed); err == nil {
		return ":" + trimmed, nil
	}
	devices, err := listMacInputDevices()
	if err != nil {
		return "", fmt.Errorf("unable to resolve --device %q: %w", trimmed, err)
	}
	needle := strings.ToLower(trimmed)
	for _, dev := range devices {
		if strings.Contains(strings.ToLower(dev.Name), needle) {
			return fmt.Sprintf(":%d", dev.Index), nil
		}
	}
	return "", fmt.Errorf("input device %q not found, list devices with: ffmpeg -f avfoundation -list_devices true -i \"\"", trimmed)
}

type macInputDevice struct {
	Index int
	Name  string
}

func listMacInputDevices() ([]macInputDevice, error) {
	cmd := exec.Command("ffmpeg", "-f", "avfoundation", "-list_devices", "true", "-i", "")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) == 0 {
			return nil, err
		}
	}
	lines := strings.Split(string(out), "\n")
	devices := parseMacDevicesPrimary(lines)
	if len(devices) == 0 {
		devices = parseMacDevicesFallback(lines)
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("no input devices found")
	}
	return devices, nil
}

func parseMacDevicesPrimary(lines []string) []macInputDevice {
	indexedName := regexp.MustCompile(`\[(\d+)\]\s*(.+)$`)
	devices := make([]macInputDevice, 0)
	seen := map[int]bool{}

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if !isLikelyAVFoundationAudioLine(line) {
			continue
		}
		if match := indexedName.FindStringSubmatch(line); len(match) == 3 {
			index, err := strconv.Atoi(strings.TrimSpace(match[1]))
			if err != nil || seen[index] {
				continue
			}
			name := cleanAVFoundationDeviceName(match[2])
			if name != "" {
				seen[index] = true
				devices = append(devices, macInputDevice{Index: index, Name: name})
			}
		}
	}

	return devices
}

func parseMacDevicesFallback(lines []string) []macInputDevice {
	indexedName := regexp.MustCompile(`\[(\d+)\]\s*(.+)$`)
	devices := make([]macInputDevice, 0)
	seen := map[int]bool{}

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if !isLikelyAVFoundationAudioLine(line) {
			continue
		}
		match := indexedName.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		index, err := strconv.Atoi(strings.TrimSpace(match[1]))
		if err != nil || seen[index] {
			continue
		}
		name := cleanAVFoundationDeviceName(match[2])
		if name == "" {
			continue
		}
		seen[index] = true
		devices = append(devices, macInputDevice{Index: index, Name: name})
	}

	return devices
}

func cleanAVFoundationDeviceName(raw string) string {
	line := strings.TrimSpace(raw)
	if idx := strings.LastIndex(line, "]"); idx >= 0 && idx+1 < len(line) {
		line = strings.TrimSpace(line[idx+1:])
	}
	line = strings.TrimSpace(strings.Trim(line, `"'`))
	if line == "" {
		return ""
	}
	if lower := strings.ToLower(line); strings.HasPrefix(lower, "avfoundation input device") {
		line = strings.TrimSpace(line[len("avfoundation input device"):])
	}
	return strings.TrimSpace(strings.Trim(line, `"'`))
}

func isLikelyAVFoundationAudioLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if !strings.Contains(lower, "avfoundation") {
		return false
	}
	if strings.Contains(lower, "audio") || strings.Contains(lower, "input") {
		return true
	}
	return !strings.Contains(lower, "video")
}

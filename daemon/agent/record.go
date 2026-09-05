package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// RecordDir is the subdirectory of a phase that holds the per-attempt record.
// It sits next to the live exchange rather than replacing it: the prompt
// templates tell the model to read and write files "in this directory", so the
// current attempt's prompt.md and out.json stay where the model expects them,
// and every attempt is copied here as it finishes.
const RecordDir = "agent"

// attemptRecord is the meta.json written beside each attempt's files.
//
// It answers, months later and without the CLI installed, the question a
// broken run actually raises: what exactly was this model asked, what did it
// answer, how long did it take, and why was that answer rejected.
type attemptRecord struct {
	Attempt   int                          `json:"attempt"`
	Provider  qaschema.AgentCapabilityName `json:"provider"`
	Phase     string                       `json:"phase"`
	RunID     string                       `json:"runId,omitempty"`
	Schema    string                       `json:"schema"`
	Status    Status                       `json:"status"`
	ExitCode  int                          `json:"exitCode"`
	TimedOut  bool                         `json:"timedOut"`
	StartedAt time.Time                    `json:"startedAt"`
	Duration  string                       `json:"duration"`
	// Command is the argv the CLI was launched with, flags only. It carries
	// no prompt and no credential: the prompt went in on stdin precisely so
	// it would not be on the process table.
	Command string `json:"command"`
	// PromptBytes and OutputBytes are sizes, not content — the content is in
	// the files next to this one.
	PromptBytes int `json:"promptBytes"`
	OutputBytes int `json:"outputBytes"`
	// ValidationErrors is why an answer was rejected, one entry per failing
	// field.
	ValidationErrors []string `json:"validationErrors,omitempty"`
	Detail           string   `json:"detail,omitempty"`
}

// recorder writes the per-attempt record into one phase's workspace.
type recorder struct {
	ws       *security.Workspace
	scrubber *security.Scrubber
}

func (r recorder) dir(attempt int) string {
	return path.Join(RecordDir, fmt.Sprintf("attempt-%d", attempt))
}

// open prepares an attempt directory and returns the log writers the provider
// gets. Both are scrubbed: a CLI that echoes the prompt back on stderr would
// otherwise put the run's fixture password in a file we keep for a week.
func (r recorder) open(attempt int) (dir string, stdout, stderr io.WriteCloser, err error) {
	dir = r.dir(attempt)
	if err := r.ws.MkdirAll(dir); err != nil {
		return "", nil, nil, fmt.Errorf("agent: prepare attempt directory: %w", err)
	}
	stdout, err = r.logFile(path.Join(dir, "stdout.log"))
	if err != nil {
		return "", nil, nil, err
	}
	stderr, err = r.logFile(path.Join(dir, "stderr.log"))
	if err != nil {
		_ = stdout.Close()
		return "", nil, nil, err
	}
	return dir, stdout, stderr, nil
}

// logFile buffers a stream and writes it on close. The whole stream is small
// — a headless CLI prints a few kilobytes — and buffering means a scrubber
// that spans a chunk boundary still catches the secret.
func (r recorder) logFile(name string) (io.WriteCloser, error) {
	return &bufferedFile{ws: r.ws, name: name, scrubber: r.scrubber}, nil
}

type bufferedFile struct {
	ws       *security.Workspace
	name     string
	scrubber *security.Scrubber
	buf      strings.Builder
}

// maxLogBytes caps one stream. A CLI stuck in a retry loop can print
// megabytes, and a workspace kept for a week must not be one of them.
const maxLogBytes = 1 << 20

func (f *bufferedFile) Write(p []byte) (int, error) {
	if f.buf.Len() < maxLogBytes {
		f.buf.Write(p)
	}
	return len(p), nil
}

func (f *bufferedFile) Close() error {
	out := f.buf.String()
	if f.scrubber != nil {
		out = f.scrubber.String(out)
	}
	return f.ws.WriteFile(f.name, []byte(out))
}

// copyInto records the prompt and the raw answer alongside the log.
func (r recorder) copyInto(dir, promptName string, prompt []byte, outputName string, output []byte) error {
	if err := r.ws.WriteFile(path.Join(dir, promptName), prompt); err != nil {
		return fmt.Errorf("agent: record prompt: %w", err)
	}
	if output == nil {
		return nil
	}
	if err := r.ws.WriteFile(path.Join(dir, outputName), output); err != nil {
		return fmt.Errorf("agent: record output: %w", err)
	}
	return nil
}

func (r recorder) meta(dir string, rec attemptRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("agent: encode attempt record: %w", err)
	}
	if r.scrubber != nil {
		data = r.scrubber.Bytes(data)
	}
	return r.ws.WriteFile(path.Join(dir, "meta.json"), append(data, '\n'))
}

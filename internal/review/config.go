package review

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/eideroliveira/aspect/internal/llm"
)

// Config is the review engine's configuration. Every field comes from the
// environment, under the names the Python CodeReviewer used, so a machine
// set up for it needs no change.
type Config struct {
	// DefaultRepo is reviewed when no repository is given:
	// CODE_REVIEWER_DEFAULT_REPO, else the working directory.
	DefaultRepo string
	// GeminiAPIKey: GEMINI_API_KEY, else GOOGLE_API_KEY, else either one
	// exported in ~/.zshenv, ~/.zshrc or ~/.bashrc (the desktop app does not
	// load a shell's rc files).
	GeminiAPIKey string
	// GeminiModel: GEMINI_MODEL (default gemini-flash-latest).
	GeminiModel string
	// Temperature: CODE_REVIEWER_TEMP (default 0.2).
	Temperature float64
	// Timeout bounds one Gemini request: CODE_REVIEWER_GEMINI_TIMEOUT, in
	// seconds (default 360). The answer arrives only after the model's
	// thinking, which depends on the role prompt more than on the diff:
	// measured 2026-10-18 on gemini-flash-latest, adversarial-reviewer
	// thinks 25-35k tokens (145-200 s) on a 25k- or a 62k-token prompt
	// alike, and 180 s failed it in 7 of 9 runs.
	Timeout time.Duration
	// MaxRetries and RetryBase: rate limits and transient 5xx are retried
	// with exponential backoff from the base, or the server's Retry-After or
	// retryDelay. CODE_REVIEWER_GEMINI_RETRIES (3) and
	// CODE_REVIEWER_GEMINI_RETRY_BASE in seconds (5).
	MaxRetries int
	RetryBase  time.Duration
	// BlockRetries: a blocked prompt is sent again up to this many times
	// (CODE_REVIEWER_GEMINI_BLOCK_RETRIES, 4); BlockFinalNoThinking turns
	// thinking off for the last of them
	// (CODE_REVIEWER_GEMINI_BLOCK_NO_THINKING, true).
	BlockRetries         int
	BlockFinalNoThinking bool
	// MaxConcurrency is how many Gemini requests one run keeps in flight
	// (CODE_REVIEWER_GEMINI_MAX_CONCURRENCY, 4; 0 is no cap). Sixteen agents
	// sending ~85k tokens each in the same second trip the per-minute token
	// quota.
	MaxConcurrency int
	// PromptBudget is the prompt size limit in tokens; a larger diff is
	// reviewed in parts (CODE_REVIEWER_GEMINI_PROMPT_BUDGET, 800000).
	PromptBudget int
	// AspectHome, an aspect checkout, supplies the agent definitions and
	// gates.yaml a repository lacks (ASPECT_HOME).
	AspectHome string
}

// ConfigFromEnv reads the configuration from the environment.
func ConfigFromEnv() Config {
	repo := os.Getenv("CODE_REVIEWER_DEFAULT_REPO")
	if repo == "" {
		repo, _ = os.Getwd()
	}
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = llm.DefaultGeminiModel
	}
	return Config{
		DefaultRepo:          repo,
		GeminiAPIKey:         geminiAPIKey(os.Getenv, homeFiles()),
		GeminiModel:          model,
		Temperature:          envFloat("CODE_REVIEWER_TEMP", 0.2),
		Timeout:              envSeconds("CODE_REVIEWER_GEMINI_TIMEOUT", 360),
		MaxRetries:           envInt("CODE_REVIEWER_GEMINI_RETRIES", 3),
		RetryBase:            envSeconds("CODE_REVIEWER_GEMINI_RETRY_BASE", 5),
		BlockRetries:         envInt("CODE_REVIEWER_GEMINI_BLOCK_RETRIES", 4),
		BlockFinalNoThinking: envBool("CODE_REVIEWER_GEMINI_BLOCK_NO_THINKING", true),
		MaxConcurrency:       envInt("CODE_REVIEWER_GEMINI_MAX_CONCURRENCY", 4),
		PromptBudget:         envInt("CODE_REVIEWER_GEMINI_PROMPT_BUDGET", 800000),
		AspectHome:           os.Getenv("ASPECT_HOME"),
	}
}

// Gemini is the client this configuration describes.
func (c Config) Gemini() *llm.Gemini {
	return llm.NewGemini(c.GeminiAPIKey,
		llm.WithGeminiModel(c.GeminiModel),
		llm.WithGeminiTemperature(c.Temperature),
		llm.WithGeminiTimeout(c.Timeout),
		llm.WithGeminiRetries(c.MaxRetries, c.RetryBase),
		llm.WithGeminiBlockRetries(c.BlockRetries, c.BlockFinalNoThinking),
	)
}

func homeFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".zshenv"), filepath.Join(home, ".zshrc"), filepath.Join(home, ".bashrc")}
}

// geminiAPIKey is the key from the environment, else the first one a
// shell file exports. A commented-out line is not a key: "#export
// GEMINI_API_KEY=..." used to win over the live one further down the file.
func geminiAPIKey(getenv func(string) string, files []string) string {
	for _, name := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		if k := strings.TrimSpace(getenv(name)); k != "" {
			return k
		}
	}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") {
				continue
			}
			if !strings.Contains(line, "GEMINI_API_KEY=") && !strings.Contains(line, "GOOGLE_API_KEY=") {
				continue
			}
			_, val, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			val = strings.Trim(val, "\"' ")
			if val != "" && !strings.HasPrefix(val, "$") {
				return val
			}
		}
	}
	return ""
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil {
		return v
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64); err == nil {
		return v
	}
	return def
}

func envSeconds(name string, def float64) time.Duration {
	return time.Duration(envFloat(name, def) * float64(time.Second))
}

func envBool(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "":
		return def
	case "true", "1", "yes":
		return true
	}
	return false
}

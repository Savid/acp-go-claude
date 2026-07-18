package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RateLimitWindow is one harness-reported subscription usage window.
type RateLimitWindow struct {
	ID          string
	UsedPercent float64
	ResetsAt    time.Time
}

// RateLimits is the harness-reported subscription usage snapshot.
type RateLimits struct {
	Windows []RateLimitWindow
}

// usageNow supplies the reference clock for reset-time year inference.
var usageNow = time.Now

// loadUsageLocation resolves the timezone name printed by the usage panel.
var loadUsageLocation = time.LoadLocation

// QueryRateLimits runs `claude /usage` non-interactively and parses the
// subscription usage panel. Every value is harness-reported; when the panel
// carries no usage windows (for example on API billing) the result is empty.
func QueryRateLimits(ctx context.Context, options Options) (RateLimits, error) {
	path, err := Discover(ctx, options.CLIPath, nil)
	if err != nil {
		return RateLimits{}, err
	}

	if options.PrepareUsageGeneration == nil {
		return RateLimits{}, fmt.Errorf("%w: claude /usage scratch generation is unavailable", ErrProcessContainmentIncomplete)
	}

	generation, err := options.PrepareUsageGeneration(ctx)
	if err != nil {
		return RateLimits{}, err
	}

	if options.AcquireUsageDiscovery == nil {
		return RateLimits{}, errors.Join(
			fmt.Errorf("%w: claude /usage native admission is unavailable", ErrProcessContainmentIncomplete),
			generation.finish(true),
		)
	}

	release, err := options.AcquireUsageDiscovery(ctx)
	if err != nil {
		return RateLimits{}, errors.Join(err, generation.finish(true))
	}

	if release == nil {
		return RateLimits{}, errors.Join(
			errors.New("admit claude /usage discovery: nil release"),
			generation.finish(true),
		)
	}

	output, err := containedClaudeOutput(
		ctx,
		path,
		[]string{"/usage", "--print", cliArgOutputFormat, "json"},
		options,
		generation,
		"claude /usage",
	)
	finishErr := generation.finish(!errors.Is(err, ErrProcessContainmentIncomplete))

	err = errors.Join(err, finishErr)
	if !errors.Is(err, ErrProcessContainmentIncomplete) {
		release()
	}

	if err != nil {
		return RateLimits{}, fmt.Errorf("run claude /usage: %w", err)
	}

	return parseUsageOutput(output, usageNow())
}

func parseUsageOutput(output []byte, now time.Time) (RateLimits, error) {
	var envelope struct {
		IsError bool   `json:"is_error"` //nolint:tagliatelle // Claude wire format.
		Result  string `json:"result"`
	}

	if err := json.Unmarshal(output, &envelope); err != nil {
		return RateLimits{}, fmt.Errorf("parse claude /usage output: %w", err)
	}

	if envelope.IsError {
		return RateLimits{}, fmt.Errorf("claude /usage failed: %s", usageErrorDetail(envelope.Result))
	}

	return RateLimits{Windows: parseUsageWindows(envelope.Result, now)}, nil
}

func usageErrorDetail(result string) string {
	detail := strings.TrimSpace(result)
	if detail == "" {
		return "empty result"
	}

	const maxDetail = 200
	if len(detail) > maxDetail {
		detail = detail[:maxDetail]
	}

	return detail
}

// usageWindowRE matches usage panel lines such as
// "Current session: 92% used · resets Jul 9, 1:40pm (Australia/Brisbane)".
var usageWindowRE = regexp.MustCompile(
	`(?m)^Current ([^:\n]+): (\d+(?:\.\d+)?)% used(?:\s*·\s*resets\s+([^\n]+?))?\s*$`,
)

func parseUsageWindows(panel string, now time.Time) []RateLimitWindow {
	matches := usageWindowRE.FindAllStringSubmatch(panel, -1)
	windows := make([]RateLimitWindow, 0, len(matches))

	for _, match := range matches {
		// usageWindowRE admits only a decimal number in this capture.
		usedPercent, _ := strconv.ParseFloat(match[2], 64)

		windows = append(windows, RateLimitWindow{
			ID:          usageWindowID(match[1]),
			UsedPercent: usedPercent,
			ResetsAt:    parseUsageReset(match[3], now),
		})
	}

	return windows
}

var usageWindowIDInvalidRE = regexp.MustCompile(`[^a-z0-9]+`)

// usageWindowID slugs a panel label ("week (all models)") into a stable
// vendor-native window id ("week-all-models").
func usageWindowID(label string) string {
	slug := usageWindowIDInvalidRE.ReplaceAllString(strings.ToLower(label), "-")

	return strings.Trim(slug, "-")
}

var usageResetLocationRE = regexp.MustCompile(`\(([^)]+)\)\s*$`)

// usageResetLayouts cover the panel's reset formats: "Jul 9, 1:40pm" and
// "Jul 11, 6am".
var usageResetLayouts = []string{"Jan 2, 3:04pm", "Jan 2, 3pm"}

// parseUsageReset parses "Jul 9, 1:40pm (Australia/Brisbane)" into a concrete
// time, inferring the year from now. It returns the zero time when the text
// cannot be parsed faithfully; the reset is then omitted rather than guessed.
func parseUsageReset(text string, now time.Time) time.Time {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}

	location := now.Location()

	if match := usageResetLocationRE.FindStringSubmatch(text); match != nil {
		loaded, err := loadUsageLocation(match[1])
		if err != nil {
			return time.Time{}
		}

		location = loaded
		text = strings.TrimSpace(strings.TrimSuffix(text, match[0]))
	}

	for _, layout := range usageResetLayouts {
		parsed, err := time.ParseInLocation(layout, text, location)
		if err != nil {
			continue
		}

		reset := time.Date(
			now.In(location).Year(), parsed.Month(), parsed.Day(),
			parsed.Hour(), parsed.Minute(), 0, 0, location,
		)

		// Reset times are always in the near future; a match far in the past
		// means the panel crossed a year boundary.
		if reset.Before(now.Add(-24 * time.Hour)) {
			reset = reset.AddDate(1, 0, 0)
		}

		return reset
	}

	return time.Time{}
}

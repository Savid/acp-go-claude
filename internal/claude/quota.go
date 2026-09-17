package claude

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"
)

const (
	quotaRejected    = "rejected"
	QuotaEventType   = "rate_limit_event"
	QuotaUnknown     = "unknown"
	QuotaFableLabel  = "Fable"
	quotaWeeklyFable = "seven_day_overage_included"
	QuotaSession     = "session"
	QuotaWeekly      = "weekly_all"
	QuotaFable       = "weekly_scoped/Fable"
	QuotaHaikuTTL    = 5 * time.Minute
	QuotaFableTTL    = 30 * time.Minute
)

// QuotaWindow retains the source time independently of cache reads.
type QuotaWindow struct {
	ID         string
	Percent    float64
	ObservedAt time.Time
	StaleAt    time.Time
	ResetsAt   time.Time
}

type QuotaResult struct {
	Exhausted        string
	Windows          []QuotaWindow
	Unavailable      bool
	NotAuthenticated bool
	RetryAfter       time.Time
}

func quotaTTL(id string) time.Duration {
	if id == QuotaFable {
		return QuotaFableTTL
	}

	return QuotaHaikuTTL
}

func quotaWindow(id string, utilization float64, reset int64, now time.Time) (QuotaWindow, bool) {
	if math.IsNaN(utilization) || math.IsInf(utilization, 0) || utilization < 0 {
		return QuotaWindow{}, false
	}

	w := QuotaWindow{ID: id, Percent: utilization * 100, ObservedAt: now, StaleAt: now.Add(quotaTTL(id))}
	if reset > 0 {
		w.ResetsAt = time.Unix(reset, 0).UTC()
		if w.ResetsAt.Before(w.StaleAt) {
			w.StaleAt = w.ResetsAt
		}
	}

	return w, true
}

func quotaHeaders(headers http.Header, fable bool, now time.Time) []QuotaWindow {
	var windows []QuotaWindow

	for _, item := range []struct{ suffix, id string }{{"5h", QuotaSession}, {"7d", QuotaWeekly}, {"7d_oi", QuotaFable}} {
		if item.id == QuotaFable && !fable {
			continue
		}

		prefix := "anthropic-ratelimit-unified-" + item.suffix

		value, err := strconv.ParseFloat(headers.Get(prefix+"-utilization"), 64)
		if err != nil {
			continue
		}

		reset, _ := strconv.ParseInt(headers.Get(prefix+"-reset"), 10, 64)
		if window, ok := quotaWindow(item.id, value, reset, now); ok {
			windows = append(windows, window)
		}
	}

	return windows
}

type quotaEventInfo struct {
	Status      string   `json:"status"`
	Type        string   `json:"rateLimitType"`
	Utilization *float64 `json:"utilization"`
	ResetsAt    int64    `json:"resetsAt"`
	Windows     map[string]struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    int64    `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

// QuotaEvent extracts reported windows and the specific exhausted window.
func QuotaEvent(event Event, fable bool, now time.Time) ([]QuotaWindow, string) {
	if event.Type != QuotaEventType {
		return nil, ""
	}

	var envelope struct {
		Info quotaEventInfo `json:"rate_limit_info"` //nolint:tagliatelle // Native stream-json member.
	}
	if json.Unmarshal(event.Raw, &envelope) != nil {
		return nil, ""
	}

	info := envelope.Info

	var windows []QuotaWindow

	for name, value := range info.Windows {
		id := quotaEventID(name, fable)
		if id != "" && value.Utilization != nil {
			if w, ok := quotaWindow(id, *value.Utilization, value.ResetsAt, now); ok {
				windows = append(windows, w)
			}
		}
	}

	id := quotaEventID(info.Type, fable)
	if id != "" && info.Utilization != nil {
		if w, ok := quotaWindow(id, *info.Utilization, info.ResetsAt, now); ok {
			windows = append(windows, w)
		}
	}

	if info.Status == quotaRejected {
		if id == "" {
			id = QuotaUnknown
		}

		return windows, id
	}

	return windows, ""
}

func quotaEventID(name string, fable bool) string {
	switch name {
	case "five_hour":
		return QuotaSession
	case "seven_day":
		return QuotaWeekly
	case quotaWeeklyFable:
		if fable {
			return QuotaFable
		}
	}

	return ""
}

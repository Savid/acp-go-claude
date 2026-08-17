package claudeacp

import "fmt"

func validateImageLimits(limits ImageLimits) error {
	checks := []struct {
		name  string
		value int64
	}{
		{name: "MaxInputBytesPerImage", value: limits.MaxInputBytesPerImage},
		{name: "MaxInputBytesPerPrompt", value: limits.MaxInputBytesPerPrompt},
		{name: "MaxOutputBytesPerImage", value: limits.MaxOutputBytesPerImage},
		{name: "MaxOutputBytesPerToolCall", value: limits.MaxOutputBytesPerToolCall},
	}

	for _, check := range checks {
		if check.value < 0 {
			return fmt.Errorf("%s must not be negative", check.name)
		}
	}

	return nil
}

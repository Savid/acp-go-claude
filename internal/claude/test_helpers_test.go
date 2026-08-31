package claude

func splitEventsForTest(events <-chan TransportEvent) (<-chan map[string]any, <-chan error) {
	messages := make(chan map[string]any, 1024)
	errs := make(chan error, 16)
	go func() {
		defer close(messages)
		defer close(errs)
		for event := range events {
			if event.Err != nil {
				errs <- event.Err

				continue
			}
			messages <- event.Message
		}
	}()

	return messages, errs
}

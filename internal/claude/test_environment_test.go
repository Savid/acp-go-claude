package claude

func withTestEnvironment(options Options) Options {
	if options.OrdinaryEnvironment == nil {
		options.OrdinaryEnvironment = OrdinaryEnvironment()
	}

	return options
}

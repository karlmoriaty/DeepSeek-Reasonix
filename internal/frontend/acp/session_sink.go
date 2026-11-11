package acp

// newBoundSink builds the sink one session streams through, with the vendor
// capabilities fixed at initialize and the root tool locations resolve against.
func (s *service) newBoundSink(id, cwd string) *updateSink {
	sink := newUpdateSink(s.conn, id)
	sink.bindCwd(cwd)
	sink.bindExtensionSurface(s.extensionSurfaceSupported())
	sink.bindJobCompletions(s.jobCompletionsSupported())
	sink.bindElicitation(s.clientCapabilities())
	return sink
}

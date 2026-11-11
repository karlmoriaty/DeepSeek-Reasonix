// vendor_caps.go — the vendor capabilities a client declares in initialize.
package acp

// extensionSurfaceSupported reports whether the connected client advertised
// reasonix.extensionSurface support in its initialize handshake.
func (s *service) extensionSurfaceSupported() bool {
	return clientExtensionSurfaceSupported(s.clientCapabilities())
}

// clientExtensionSurfaceSupported tolerantly parses the client's vendor
// capability block: _meta["reasonix.io"]["extensionSurface"]["supported"] must
// be an explicit true. Absent keys, wrong shapes, or a malformed block all
// mean unsupported — the sink then sends only the text fallback.
func clientExtensionSurfaceSupported(caps ClientCapabilities) bool {
	return clientVendorCapability(caps, "extensionSurface")
}

// statusUpdatesSupported reports whether the client declared
// reasonix.statusUpdates, the opt-in for the unsolicited vendor notifications
// (_reasonix.io/session/status_update and mcpStatus_update). A client that
// pulls the same state with the matching requests needs no declaration.
func (s *service) statusUpdatesSupported() bool {
	return clientVendorCapability(s.clientCapabilities(), "statusUpdates")
}

// jobCompletionsSupported reports whether the client declared
// reasonix.jobCompletions, the opt-in for the _reasonix.io/job_completed
// vendor update. The update is only useful to a client that parks a session
// when a turn ends awaiting background jobs; undeclared clients keep the old
// behavior of reading results through wait or bash_output.
func (s *service) jobCompletionsSupported() bool {
	return clientVendorCapability(s.clientCapabilities(), "jobCompletions")
}

func clientVendorCapability(caps ClientCapabilities, name string) bool {
	vendor, ok := caps.Meta["reasonix.io"].(map[string]any)
	if !ok {
		return false
	}
	capability, ok := vendor[name].(map[string]any)
	if !ok {
		return false
	}
	supported, _ := capability["supported"].(bool)
	return supported
}

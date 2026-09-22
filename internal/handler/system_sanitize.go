package handler

// System passthrough.
//
// The gateway relays client content verbatim (the fidelity default): system
// items, including any cc_entrypoint markers a coding agent attaches, are
// forwarded without rewriting. The previous auto/strip pipeline was removed
// when the fidelity default became the only behavior; if upstream filtering is
// ever needed again, reintroduce it behind an explicit config knob rather than
// resurrecting this file.

// sanitizeSystemItems keeps the call-site shape. Fidelity is always on, so the
// items are never rewritten.
func sanitizeSystemItems(system SystemItems) (SystemItems, bool) {
	return system, false
}

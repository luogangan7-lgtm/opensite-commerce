// Responsibility: temporary C-GOV2 opaque-output mutation probe.
// Dependencies: IdentityRunner; callers: export surface CI only.
// Invariant: this deliberate unsafe export must fail the gate and never merge.
package pgxadapter

func (r IdentityRunner) Raw() any { return r.pool }

// Responsibility: deliberate negative composition fixture, never merged.
// Dependencies: Payments live constructor; caller: normal CI guard only.
package main

import "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"

var forbiddenLiveComposition = payments.NewStripeLiveSmoke

func main() {}

// Responsibility: temporary static fault probe, removed after CI evidence.
// Dependencies: test sender constructor symbol; never invoked.
// Invariant deliberately violated: production must never retain this function.
package notifications

var smtpCompositionProbe = ExternalTestSenderFromEnv

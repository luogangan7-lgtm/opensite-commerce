# Build provenance

CI seals `apparel-dist/build-receipt.json` after compiling the Worker. The receipt contains the checkout SHA, GitHub run ID and every input file digest. CI logs print `BUILD_RECEIPT_SHA256`; this digest must be obtained independently from the successful, exact-SHA apparel job, not copied from the downloaded artifact.

Before running `copy-images.ts`, verify the Actions run's head SHA and successful apparel job and supply that log digest through `APPAREL_BUILD_RECEIPT_SHA256`. Packaging rejects missing receipts, changed input bytes, stale checkouts and forged receipt SHAs before writing images. The deploy manifest records build SHA/run/digest separately from packager SHA. This is an integrity binding to a trusted CI log, not a cryptographic artifact attestation or production deployment approval.

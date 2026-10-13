// Responsibilities: prove the public ledger posting seam is implemented by the
// composition entry point. Dependencies: ledger/contract and ledger; compile gate
// callers; no database or transaction lifecycle is exercised here.
package contract_test

import (
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
)

var _ contract.Poster = ledger.New()

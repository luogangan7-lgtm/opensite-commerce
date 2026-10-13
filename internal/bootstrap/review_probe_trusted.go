// REVIEW PROBE 96-r8: a second composition site naming the trusted seam must be rejected (G13).
package bootstrap

import "github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"

var reviewProbeTrusted = (*pgxadapter.QueryRunner).RunTrustedTx

// Test fixture: a same-package non-test file carries an omitted multi-name const list.
package fail

const (
	crossCreate, crossRole = "CREATE ", "ROLE fixture_cross_omitted"
	crossPrefix, crossTail // omitted: repeats both values by position
)

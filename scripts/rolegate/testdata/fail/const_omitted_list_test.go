// Test fixture: an omitted const expression list repeats every expression, not just the last.
package fail

const (
	createPrefix, roleTail = "CREATE ", "ROLE fixture_omitted_list"
	rolePrefix, roleSQL    // omitted: repeats ("CREATE ", "ROLE fixture_omitted_list")
)

var omittedListStatement = rolePrefix + roleSQL

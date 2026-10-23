// Test fixture: a same-package constant declared in a non-test file is folded.
package fail

const sharedRoleSQL = "DROP ROLE fixture_cross_file"

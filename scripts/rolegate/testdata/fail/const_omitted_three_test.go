// Test fixture: a three-expression const list omitted later binds by position.
package fail

const (
	createWord, spaceWord, roleWord = "CREATE", " ", "ROLE fixture_omitted_three"
	omittedCreate, omittedSpace, omittedRole
)

var omittedThreeStatement = omittedCreate + omittedSpace + omittedRole

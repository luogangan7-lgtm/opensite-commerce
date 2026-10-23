// Test fixture: constants declared inside a function body are folded too.
package fail

func localConstStatement(ctx, db any) {
	const prefix = "CREATE "
	const keyword = "ROLE "
	db.Exec(ctx, prefix+keyword+"fixture_local_const")
}

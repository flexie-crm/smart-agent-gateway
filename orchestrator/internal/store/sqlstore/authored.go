package sqlstore

// The author columns, as every read selects them and every write sets them.
//
// Named once because they appear on nineteen tables: a typo in one of nineteen
// hand-written column lists is a column that silently reads as empty, and the
// only symptom is a screen that says nobody made something. They are constants,
// so concatenating them into a query is still a compile-time constant and still
// a valid sqldb.Statement (KB/14).
//
// COALESCE on the ids, because the column is nullable and the model's field is
// not: a person who has been deleted reads as 0, which is what "no id" means
// everywhere above the store. The names need no coalescing; they are NOT NULL
// with an empty default.
// authoredColumns is for a row that can be edited: who made it, who last
// changed it.
//
// There is no companion for a row that CANNOT be edited (a skill version),
// because every read of one of those is a join and needs the columns qualified
// by their table, which an unqualified list cannot be. One of them coalesces
// the name as well, for the outer join that reaches a skill with no version
// yet. They write their own lists, and a constant nothing can use is worse
// than no constant: it reads as the shared thing while sharing nothing.
const authoredColumns = `COALESCE(created_by, 0), created_by_name,
	       COALESCE(updated_by, 0), updated_by_name`

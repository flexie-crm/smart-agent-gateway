package model

// Actor is who or what performed a write.
//
// It is a parameter of the write rather than a field of the thing being
// written: what arrived and who sent it are two different facts, and only one
// of them is in the payload.
//
// UserID is zero when it was not a person, which is how an agent's write is
// recorded. Name is FROZEN into the row: for a person it is their full name,
// and for anything else it is what should be printed instead (an agent, a
// model). It survives the person, because a record of who did something that
// disappears with their account rewrites the history every time somebody
// leaves.
type Actor struct {
	UserID int64
	Name   string
}

// Nobody is the actor for a write with no author: a first run seeding its own
// data, a migration, a test that is not about authorship. It records no id and
// no name rather than inventing one.
func Nobody() Actor { return Actor{} }

// Authored is who made a row, embedded by every table that holds something
// somebody decided.
//
// Embedded rather than repeated, so the concept has one definition and a table
// that wants it costs one line. Embedded WITHOUT a json tag, which flattens its
// fields into the object around it: the wire format is plain fields, exactly as
// if they were written out on every struct. There is a test for that, because
// it is the kind of thing that stays true until somebody adds a tag.
//
// The id is zero when nobody was recorded, which covers three real cases: a row
// written before any of this was kept, a row written by something that is not a
// person, and a person since deleted. The NAME survives all three, so it is
// what a screen reads.
type Authored struct {
	CreatedBy     int64  `json:"created_by"`
	CreatedByName string `json:"created_by_name"`
}

// Made records who created the row.
func (a *Authored) Made(by Actor) {
	a.CreatedBy, a.CreatedByName = by.UserID, by.Name
}

// Edited is who last changed a row, embedded ALONGSIDE Authored by the tables
// whose rows can be changed.
//
// Separate from Authored, and not four fields in one struct, because plenty of
// rows here cannot be edited at all: a skill version is immutable by design. On
// those, an updated pair would be two columns nothing ever writes and two
// fields on the wire that are always zero, which is a promise the data does not
// keep.
type Edited struct {
	UpdatedBy     int64  `json:"updated_by"`
	UpdatedByName string `json:"updated_by_name"`
}

// Changed records who made an edit. Who MADE the row is left alone: that is the
// one fact an edit cannot change.
func (e *Edited) Changed(by Actor) {
	e.UpdatedBy, e.UpdatedByName = by.UserID, by.Name
}

// Unchanged is the actor to pass for a write NOBODY made: a key rotation
// resealing every credential, a background reconciliation, a machine reporting
// its own address. It hands back whoever is already on the row, so the write
// leaves the record exactly as it was.
//
// It exists because Nobody() would be wrong for those: it does not mean "leave
// it alone", it means "nobody did this", and writing it would ERASE the person
// who last really did something. A maintenance pass that quietly blanks the
// author of every row it touches is worse than one that records nothing.
func (e *Edited) Unchanged() Actor {
	return Actor{UserID: e.UpdatedBy, Name: e.UpdatedByName}
}

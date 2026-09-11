package state

import (
	"context"
	"testing"
	"time"
)

func ctx() context.Context { return context.Background() }

func TestAValueComesBackAsItWentIn(t *testing.T) {
	store := NewMemory()
	if err := Write(ctx(), store, "who", "ada", 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	value, found, err := Read[string](ctx(), store, "who")
	if err != nil || !found || value != "ada" {
		t.Fatalf("got %q found=%v err=%v", value, found, err)
	}

	if err := store.Delete(ctx(), "who"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := store.Get(ctx(), "who"); found {
		t.Fatal("a deleted value is still there")
	}
	// Deleting what is not there is not a failure: a caller cleaning up should
	// not have to know whether there was anything to clean.
	if err := store.Delete(ctx(), "never-written"); err != nil {
		t.Fatalf("deleting nothing failed: %v", err)
	}
}

// What is handed out is a copy. Without that, a caller changing what it read
// changes what everybody else reads, which is a bug that only appears when two
// things read the same key.
func TestWhatIsHandedOutIsACopy(t *testing.T) {
	store := NewMemory()
	_ = store.Set(ctx(), "greeting", []byte("hello"), 0)
	value, _, _ := store.Get(ctx(), "greeting")
	value[0] = 'H'
	again, _, _ := store.Get(ctx(), "greeting")
	if string(again) != "hello" {
		t.Fatalf("the stored value was changed from outside: %q", again)
	}
}

// A value past its time is not there. It is the same answer as never having
// been written, because a caller cannot act on "it was here a moment ago".
func TestAValuePastItsTimeIsNotThere(t *testing.T) {
	store := NewMemory()
	clock := time.Now()
	store.now = func() time.Time { return clock }

	_ = store.Set(ctx(), "brief", []byte("here"), time.Minute)
	if _, found, _ := store.Get(ctx(), "brief"); !found {
		t.Fatal("it should be there before its time")
	}
	clock = clock.Add(time.Minute + time.Second)
	if _, found, _ := store.Get(ctx(), "brief"); found {
		t.Fatal("it is still there after its time")
	}
}

func TestReadingWhatIsNotThereAndWhatIsWrong(t *testing.T) {
	store := NewMemory()
	if _, found, err := Read[int](ctx(), store, "nothing"); found || err != nil {
		t.Fatalf("got found=%v err=%v for a key nobody wrote", found, err)
	}
	_ = store.Set(ctx(), "words", []byte(`"a string"`), 0)
	if _, _, err := Read[int](ctx(), store, "words"); err == nil {
		t.Fatal("a value of the wrong shape was read as a number")
	}
}

// A mistake somewhere writing a key per request must not grow this for ever.
// What is swept is only what has expired: a value with no time on it is
// somebody's.
func TestExpiredValuesAreSweptWhenItGrows(t *testing.T) {
	store := NewMemory()
	store.keys = 100
	clock := time.Now()
	store.now = func() time.Time { return clock }

	for i := 0; i < 150; i++ {
		_ = store.Set(ctx(), time.Duration(i).String(), []byte("x"), time.Minute)
	}
	_ = store.Set(ctx(), "kept", []byte("x"), 0)
	clock = clock.Add(2 * time.Minute)
	_ = store.Set(ctx(), "one more", []byte("x"), time.Minute)

	store.mu.Lock()
	left := len(store.values)
	store.mu.Unlock()
	if left > 2 {
		t.Fatalf("%d values are still held after they expired", left)
	}
	if _, found, _ := store.Get(ctx(), "kept"); !found {
		t.Fatal("a value with no time on it was swept")
	}
}

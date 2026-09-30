//go:build unix

package ledger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// flock is this package's only mutual-exclusion primitive, and it is advisory:
// nothing stops a process that has not agreed to take it, which is exactly why
// every writer goes through Ledger.Lock.
//
// It is also the reason a crashed caf needs no janitor. flock is owned by the
// open file description, so the kernel drops it when the holder dies — a lock
// left behind by a killed process is not stale, it is gone. That property is
// what lets a port reservation be *held* for the life of a session and be
// reclaimed the moment it is not, with nothing deciding whose lock is stale.
type Lock struct {
	file *os.File
	name string
}

// ErrHeld is a lock somebody else has. It is a distinct sentinel because the
// fix differs from every other ledger failure: nothing is broken, the answer is
// to try again or to leave the other process alone.
var ErrHeld = errors.New("held")

// Lock takes the named lock and waits for it.
//
// The name is a file name inside the ledger, chosen by the caller: "ledger" for
// the ledger's own mutations, and a port number for that port's reservation.
// Sharing one primitive means one implementation of the thing that has to be
// right.
func (l *Ledger) Lock(name string) (*Lock, error) {
	return l.lockWith(name, 0)
}

// TryLock takes the named lock or reports that somebody else has it. It is how
// a prober answers "is this port reserved by a live session" without waiting,
// because a CLI that hangs behind a lock it cannot see is a CLI that looks
// broken.
func (l *Ledger) TryLock(name string) (*Lock, error) {
	return l.lockWith(name, syscall.LOCK_NB)
}

func (l *Ledger) lockWith(name string, flags int) (*Lock, error) {
	file, err := l.openLockFile(name)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|flags)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		break
	}
	if err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s is locked by another caf", ErrHeld, name)
		}
		return nil, fmt.Errorf("%w: take the %s lock: %w", ErrNoLedger, name, err)
	}
	return &Lock{file: file, name: name}, nil
}

// Record writes what is holding the lock. It is for a report and for a person
// looking at a directory — it is never consulted to decide whether the lock is
// held. Liveness is the flock and nothing else, because a file that says "held
// by session X" is a file that is wrong the moment X exits.
func (k *Lock) Record(text string) error {
	if err := k.file.Truncate(0); err != nil {
		return err
	}
	if _, err := k.file.WriteAt([]byte(text+"\n"), 0); err != nil {
		return err
	}
	return k.file.Sync()
}

// Release drops the lock. It is safe to call twice.
func (k *Lock) Release() error {
	if k == nil || k.file == nil {
		return nil
	}
	file := k.file
	k.file = nil
	unlock := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlock != nil {
		return fmt.Errorf("%w: release the %s lock: %w", ErrNoLedger, k.name, unlock)
	}
	return closeErr
}

// hold serialises the ledger's own mutations.
func (l *Ledger) hold() (func() error, error) {
	lock, err := l.Lock(ledgerLockName)
	if err != nil {
		return nil, err
	}
	return lock.Release, nil
}

func (l *Ledger) holdNonBlocking() (func() error, error) {
	lock, err := l.TryLock(ledgerLockName)
	if err != nil {
		return nil, err
	}
	return lock.Release, nil
}

// ledgerLockName is the lock every mutation of the ledger itself takes. Port
// reservations take their own, one per port, so that a hundred ports in the
// block are a hundred locks rather than one queue.
const ledgerLockName = "ledger"

func (l *Ledger) openLockFile(name string) (*os.File, error) {
	path := filepath.Join(l.dir, "locks", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("%w: create %s: %w", ErrNoLedger, filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %w", ErrNoLedger, path, err)
	}
	return file, nil
}

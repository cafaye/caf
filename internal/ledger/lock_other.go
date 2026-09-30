//go:build !unix

package ledger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The standard library exposes flock on the unix systems and not on Windows, and
// caf ships a Windows binary (.goreleaser.yml lists goos: windows). Rather than
// add a module to go.mod for one syscall, this file uses the atomic exclusive
// create that every platform provides.
//
// It is a weaker primitive and the difference is stated rather than hidden: an
// exclusive-create lock is the *existence of a file*, so a process killed
// between the create and the close leaves a lock nobody can release. flock has
// no such state because the kernel owns it. A ledger on Windows therefore needs
// a person to delete the lock file after a crash, and every error here says so
// in the sentence rather than waiting forever for a lock that will not come
// back on its own.
type Lock struct {
	file *os.File
	name string
}

// ErrHeld is a lock somebody else has. It is a distinct sentinel because the fix
// differs from every other ledger failure: nothing is broken, the answer is to
// try again or to leave the other process alone.
var ErrHeld = errors.New("held")

func (l *Ledger) Lock(name string) (*Lock, error) {
	path, err := l.lockPath(name)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s is locked by another caf, and on this platform a lock left behind by a killed caf has to be removed by hand (rm %s)", ErrHeld, name, path)
		}
		return nil, fmt.Errorf("%w: take the %s lock: %w", ErrNoLedger, name, err)
	}
	return &Lock{file: file, name: name, path: path}, nil
}

// TryLock is Lock on a platform where the two are the same primitive: an
// exclusive create is already a non-blocking try.
func (l *Ledger) TryLock(name string) (*Lock, error) { return l.Lock(name) }

// Record writes what is holding the lock. It is for a report and for a person
// looking at a directory — it is never consulted to decide whether the lock is
// held. Liveness is the lock and nothing else, because a file that says "held
// by session X" is a file that is wrong the moment X exits.
func (k *Lock) Record(text string) error {
	if err := k.file.Truncate(0); err != nil {
		return err
	}
	_, err := k.file.WriteAt([]byte(strings.TrimRight(text, "\n")+"\n"), 0)
	return err
}

// Release drops the lock, by closing the file and removing it.
func (k *Lock) Release() error {
	if k == nil || k.file == nil {
		return nil
	}
	file, path := k.file, k.path
	k.file = nil
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("%w: release the %s lock: %w", ErrNoLedger, k.name, removeErr)
	}
	return nil
}

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

func (l *Ledger) lockPath(name string) (string, error) {
	path := filepath.Join(l.dir, "locks", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("%w: create %s: %w", ErrNoLedger, filepath.Dir(path), err)
	}
	return path, nil
}

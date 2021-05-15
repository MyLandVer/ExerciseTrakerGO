package app

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jovandeginste/workout-tracker/pkg/database"
)

var (
	ErrWorker          = errors.New("worker error")
	ErrNothingImported = errors.New("nothing imported")
)

const (
	FileAddDelay = -1 * time.Minute
	WorkerDelay  = 1 * time.Minute
)

func (a *App) BackgroundWorker() {
	l := a.logger.With("module", "worker")

	for {
		l.Info("Worker started...")

		a.updateWorkout(l)
		a.autoImports(l)

		l.Info("Worker finished...")
		time.Sleep(WorkerDelay)
	}
}

func (a *App) autoImports(l *slog.Logger) {
	var uID []int

	q := a.db.Model(&database.User{}).Pluck("ID", &uID)
	if err := q.Error; err != nil {
		l.Error(ErrWorker.Error() + ": " + err.Error())
	}

	for _, i := range uID {
		if err := a.autoImportForUser(l, i); err != nil {
			l.Error(ErrWorker.Error() + ": " + err.Error())
		}
	}
}

func (a *App) autoImportForUser(l *slog.Logger, userID int) error {
	u, err := database.GetUserByID(a.db, userID)
	if err != nil {
		return err
	}

	userLogger := l.With("user", u.Username)

	ok, err := u.Profile.CanImportFromDirectory()
	if err != nil {
		return fmt.Errorf("could not use auto-import dir %v for user %v: %w", u.Profile.AutoImportDirectory, u.Username, err)
	}

	if !ok {
		return nil
	}

	userLogger.Info("Importing from '" + u.Profile.AutoImportDirectory + "'")

	// parse all files in the directory, non-recusive
	files, err := filepath.Glob(filepath.Join(u.Profile.AutoImportDirectory, "*"))
	if err != nil {
		return err
	}

	for _, path := range files {
		pathLogger := userLogger.With("path", path)

		if err := a.importForUser(pathLogger, u, path); err != nil {
			pathLogger.Error("Could not import: " + err.Error())
		}
	}

	return nil
}

func (a *App) importForUser(logger *slog.Logger, u *database.User, path string) error {
	// Get file info for path
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if !fileCanBeImported(path, info) {
		return nil
	}

	if importErr := a.importFile(logger, u, path); importErr != nil {
		logger.Error("Could not import: " + importErr.Error())
		return moveImportFile(logger, u.Profile.AutoImportDirectory, path, "failed")
	}

	return moveImportFile(logger, u.Profile.AutoImportDirectory, path, "done")
}

func moveImportFile(logger *slog.Logger, dir, path, statusDir string) error {
	destDir := filepath.Join(dir, statusDir)

	logger.Info("Moving to '" + destDir + "'")

	// If destDir does not exist, create it
	if _, err := os.Stat(destDir); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(destDir, 0o755); err != nil {
			return err
		}
	}

	if err := os.Rename(path, filepath.Join(destDir, filepath.Base(path))); err != nil {
		return err
	}

	logger.Info("Moved to '" + destDir + "'")

	return nil
}

func (a *App) importFile(logger *slog.Logger, u *database.User, path string) error {
	logger.Info("Importing path")

	dat, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	w, err := u.AddWorkout(a.db, database.WorkoutTypeAutoDetect, "", path, dat)
	if err != nil {
		return err
	}

	if w == nil {
		return ErrNothingImported
	}

	logger.Info("Finished import.")

	return nil
}

func fileCanBeImported(p string, i os.FileInfo) bool {
	if i.IsDir() {
		return false
	}

	// If file was changed within the last minute, don't import it
	if i.ModTime().After(time.Now().Add(FileAddDelay)) {
		return false
	}

	for _, e := range []string{".gpx", ".fit", ".tcx"} {
		if filepath.Ext(p) == e {
			return true
		}
	}

	return false
}

func (a *App) updateWorkout(l *slog.Logger) {
	var wID []int

	q := a.db.Model(&database.Workout{}).Where(&database.Workout{Dirty: true}).Limit(1000).Pluck("ID", &wID)
	if err := q.Error; err != nil {
		l.Error("Worker error: " + err.Error())
	}

	for _, i := range wID {
		l.Info(fmt.Sprintf("Updating workout %d", i))

		if err := a.UpdateWorkout(i); err != nil {
			l.Error("Worker error: " + err.Error())
		}
	}
}

func (a *App) UpdateWorkout(i int) error {
	w, err := database.GetWorkoutWithGPX(a.db, i)
	if err != nil {
		return err
	}

	return w.UpdateData(a.db)
}

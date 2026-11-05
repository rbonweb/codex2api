package database

import "context"

func (db *DB) initializeRuntimeCacheScope(ctx context.Context) error {
	revision, err := db.GetAPIKeyAuthRevision(ctx)
	if err != nil {
		return err
	}
	db.runtimeCacheScope = revision.Namespace
	return nil
}

// RuntimeCacheScope isolates different databases that use the same Redis, and does not include the raw connection credentials.
func (db *DB) RuntimeCacheScope() string {
	if db == nil {
		return ""
	}
	return db.runtimeCacheScope
}

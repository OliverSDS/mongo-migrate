// Package migrate allows to perform versioned migrations in your MongoDB.
package migrate

import (
	"context"
	"log"
	"time"

	"github.com/globalsign/mgo/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type versionRecord struct {
	Version     uint64
	Description string `bson:",omitempty"`
	Timestamp   time.Time
}

const defaultMigrationsCollection = "migrations"

// AllAvailable used in "Up" or "Down" methods to run all available migrations.
const AllAvailable = -1

// Migrate is type for performing migrations in provided database.
// Database versioned using dedicated collection.
// Each migration applying ("up" and "down") adds new document to collection.
// This document consists migration version, migration description and timestamp.
// Current database version determined as version in latest added document (biggest "_id") from collection mentioned above.
type Migrate struct {
	db                   *mongo.Database
	migrations           []Migration
	migrationsCollection string
	logger               *log.Logger
}

func NewMigrate(db *mongo.Database, migrations ...Migration) *Migrate {
	internalMigrations := make([]Migration, len(migrations))
	copy(internalMigrations, migrations)
	return &Migrate{
		db:                   db,
		migrations:           internalMigrations,
		migrationsCollection: defaultMigrationsCollection,
	}
}

// SetMigrationsCollection replaces name of collection for storing migration information.
// By default it is "migrations".
func (m *Migrate) SetMigrationsCollection(name string) {
	m.migrationsCollection = name
}

// SetLogger set a logger
func (m *Migrate) SetLogger(l *log.Logger) {
	m.logger = l
}

func (m *Migrate) isCollectionExist(name string) (bool, error) {
	colls, err := m.db.ListCollectionNames(context.Background(), bson.D{})
	if err != nil {
		return false, err
	}
	for _, v := range colls {
		if name == v {
			return true, nil
		}
	}
	return false, nil
}

func (m *Migrate) createCollectionIfNotExist(name string) error {
	exist, err := m.isCollectionExist(name)
	if err != nil {
		return err
	}
	if exist {
		return nil
	}
	// I had a problem here with bson.D: mongo returned error like "command not found: '0'"
	result := m.db.RunCommand(context.Background(), struct {
		Create string `bson:"create"`
	}{
		Create: name,
	}, nil)

	return result.Err()
}

// Version returns current database version and comment.
func (m *Migrate) Version() (uint64, string, error) {
	if err := m.createCollectionIfNotExist(m.migrationsCollection); err != nil {
		return 0, "", err
	}

	var rec versionRecord
	opts := options.FindOptions{
		Sort: bson.M{"_id": -1},
	}
	// find record with greatest id (assuming it`s latest also)
	cursor, err := m.db.Collection(m.migrationsCollection).Find(context.Background(), bson.M{}, &opts)
	if err == mongo.ErrNoDocuments {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	decodeErr := cursor.Decode(&rec)
	if decodeErr != nil {
		return 0, "", decodeErr
	}
	return rec.Version, rec.Description, nil
}

// Applied check if version was applied
func (m *Migrate) Applied(version uint64) bool {
	if err := m.createCollectionIfNotExist(m.migrationsCollection); err != nil {
		return false
	}
	var rec versionRecord
	cursor, err := m.db.Collection(m.migrationsCollection).Find(context.Background(), bson.M{"version": version})
	if err == mongo.ErrNoDocuments {
		return false
	}
	if err != nil {
		return false
	}
	decodeErr := cursor.Decode(&rec)
	if decodeErr != nil {
		return false
	}
	return true
}

// SetVersion forcibly changes database version to provided.
func (m *Migrate) SetVersion(version uint64, description string) error {
	_, err := m.db.Collection(m.migrationsCollection).InsertOne(context.Background(), versionRecord{
		Version:     version,
		Timestamp:   time.Now().UTC(),
		Description: description,
	})
	return err
}

// Up performs "up" migrations to latest available version.
// If n<=0 all "up" migrations with newer versions will be performed.
// If n>0 only n migrations with newer version will be performed.
func (m *Migrate) Up(n int) error {
	if n <= 0 || n > len(m.migrations) {
		n = len(m.migrations)
	}
	migrationSort(m.migrations)

	for i, p := 0, 0; i < len(m.migrations) && p < n; i++ {
		migration := m.migrations[i]
		if m.Applied(migration.Version) || migration.Up == nil {
			continue
		}
		p++
		if err := migration.Up(m.db); err != nil {
			return err
		}
		if m.logger != nil {
			m.logger.Printf("MIGRATED UP: %d %s\n", migration.Version, migration.Description)
		}
		if err := m.SetVersion(migration.Version, migration.Description); err != nil {
			return err
		}
	}
	return nil
}

// Down performs "down" migration to oldest available version.
// If n<=0 all "down" migrations with older version will be performed.
// If n>0 only n migrations with older version will be performed.
func (m *Migrate) Down(n int) error {
	currentVersion, _, err := m.Version()
	if err != nil {
		return err
	}
	if n <= 0 || n > len(m.migrations) {
		n = len(m.migrations)
	}
	migrationSort(m.migrations)

	for i, p := len(m.migrations)-1, 0; i >= 0 && p < n; i-- {
		migration := m.migrations[i]
		if migration.Version > currentVersion || migration.Down == nil {
			continue
		}
		p++
		if err := migration.Down(m.db); err != nil {
			return err
		}

		var prevMigration Migration
		if i == 0 {
			prevMigration = Migration{Version: 0}
		} else {
			prevMigration = m.migrations[i-1]
		}
		if m.logger != nil {
			m.logger.Printf("MIGRATED DOWN: %d %s\n", migration.Version, migration.Description)
		}
		if err := m.SetVersion(prevMigration.Version, prevMigration.Description); err != nil {
			return err
		}
	}
	return nil
}

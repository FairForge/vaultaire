package database

import (
    "os"
)

func GetTestDSN() string {
    if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
        return dsn
    }
    return "postgres://vaultaire:vaultaire_dev@localhost/vaultaire_dev?sslmode=disable"
}

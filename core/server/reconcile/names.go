package reconcile

// DatabaseSecret is the Secret can-core writes into a module namespace whose manifest requires a
// postgres or mariadb database (DB_HOST, DB_PORT, DB_NAME, DB_USER, DB_PASSWORD, DATABASE_URL).
// The installer writes it; backup hook Jobs read it.
const DatabaseSecret = "can-db"

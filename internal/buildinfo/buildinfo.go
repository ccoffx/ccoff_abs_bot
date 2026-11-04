package buildinfo

// Set by the release build. Database compatibility is an explicit release gate.
var Version = "dev"
var Commit = "unknown"

const SchemaCompatibility = "1"

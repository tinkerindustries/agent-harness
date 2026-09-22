# Dialect.AddressID is unused

The `Dialect` interface in `internal/stdiosession/dialect.go` has an `AddressID` method that
nothing calls. `AppendResult` decides where each dialect puts the id. Remove the method and its
three implementations.

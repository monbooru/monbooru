package tagger

import "github.com/monbooru/monbooru/internal/shared/worker"

// LoadCatalog is the workers' catalogs: monbooru never fetches these URLs
// itself, and the worker neither. Settings shows the command, to run on
// the worker's machine.
func LoadCatalog() []worker.CatalogEntry { return currentPool().Catalog() }

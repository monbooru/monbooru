package tagger

import "github.com/monbooru/monbooru/internal/shared/worker"

// LoadCatalog is the workers' catalogs; neither monbooru nor a worker
// fetches their URLs.
func LoadCatalog() []worker.CatalogEntry { return CurrentPool().Catalog() }

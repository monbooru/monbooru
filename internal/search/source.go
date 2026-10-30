package search

import (
	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/relations"
)

type Source struct {
	DB             *db.DB
	Tree           *relations.BKTree
	Tallies        *counts.Tallies
	Cache          *AdjacencyCache
	LookupBackends []string
}

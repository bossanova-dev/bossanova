package server

import "github.com/recurser/bossd/internal/domain"

// evidenceBucketOpener reads the upload credential fresh for each publish operation.
type evidenceBucketOpener interface {
	OpenEvidenceBucket() (domain.EvidenceBucket, error)
}

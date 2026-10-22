package domain

import "github.com/recurser/bossalib/models"

// Session stays shared with boss and bossalib/vcs until the domain split completes.
type Session = models.Session

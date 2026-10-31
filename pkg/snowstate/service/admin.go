package service

import "context"

// Retention configures GC.
type Retention struct {
	KeepVersions int
	KeepDays     int
	DeletedDays  int
}

// AdminService is the admin half of StateService.
type AdminService interface {
	Ping(ctx context.Context) error
}

// Ping checks that storage is reachable.
func (s *Service) Ping(ctx context.Context) error { return s.st.Ping(ctx) }

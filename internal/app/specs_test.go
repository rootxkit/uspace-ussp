package app_test

import (
	"slices"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/app/api"
	"github.com/rootxkit/uspace-ussp/internal/app/dsssync"
	"github.com/rootxkit/uspace-ussp/internal/app/monitor"
	"github.com/rootxkit/uspace-ussp/internal/app/proc"
	"github.com/rootxkit/uspace-ussp/internal/app/ridsp"
	"github.com/rootxkit/uspace-ussp/internal/app/telemetryingest"
	"github.com/rootxkit/uspace-ussp/internal/app/trafficws"
	"github.com/rootxkit/uspace-ussp/internal/app/tsdbwriter"
	"github.com/rootxkit/uspace-ussp/internal/config"
)

// The seven specs match the process table of docs/PLAN.md §3.1: only
// api opens PostgreSQL (D6: hot-path processes never do), only api and
// tsdb-writer open TimescaleDB, every process needs NATS, and only the
// two database owners have a migrate subcommand (D5).
func TestSpecsFollowThePlan(t *testing.T) {
	specs := []proc.Spec{api.Spec, telemetryingest.Spec, ridsp.Spec, monitor.Spec, trafficws.Spec, dsssync.Spec, tsdbwriter.Spec}
	var names []string
	for _, s := range specs {
		names = append(names, s.Process)
		if s.NATS != proc.Required {
			t.Errorf("%s: NATS is not required", s.Process)
		}
		if (s.Postgres != proc.NotUsed) != (s.Process == config.ProcessAPI) {
			t.Errorf("%s: postgres need %d", s.Process, s.Postgres)
		}
		usesTS := s.Process == config.ProcessAPI || s.Process == config.ProcessTSDBWriter
		if (s.TimescaleDB != proc.NotUsed) != usesTS {
			t.Errorf("%s: timescaledb need %d", s.Process, s.TimescaleDB)
		}
		if s.Migrate != usesTS {
			t.Errorf("%s: migrate %v", s.Process, s.Migrate)
		}
		withRoutes := s.Process == config.ProcessAPI || s.Process == config.ProcessRIDSP || s.Process == config.ProcessTSDBWriter ||
			s.Process == config.ProcessTelemetryIngest || s.Process == config.ProcessMonitor
		if (s.Routes != nil) != withRoutes {
			t.Errorf("%s: routes before their work package (api's arrive with WP-2, rid-sp's with WP-3, tsdb-writer's workers with WP-6, telemetry-ingest's with WP-8, monitor's workers with WP-10)", s.Process)
		}
	}
	if !slices.Equal(names, config.Processes) {
		t.Fatalf("processes %v, want %v", names, config.Processes)
	}
	if api.Spec.Postgres != proc.Required || api.Spec.TimescaleDB != proc.Optional || tsdbwriter.Spec.TimescaleDB != proc.Optional {
		t.Fatal("api needs PostgreSQL and serves reads without TimescaleDB; tsdb-writer queues and spills without it (PLAN §3.1)")
	}
}

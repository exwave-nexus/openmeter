package simulation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/openmeterio/openmeter/cmd/jobs/internal"
	"github.com/openmeterio/openmeter/openmeter/billing/worker/subscriptionsync/reconciler"
	"github.com/openmeterio/openmeter/pkg/clock"
)

const (
	defaultAddress   = "0.0.0.0:8890"
	defaultLookback  = 100 * 365 * 24 * time.Hour
	defaultMaxPasses = 4
	defaultTimeout   = 30 * time.Second
	defaultBatchSize = 16
)

type drainRequest struct {
	MaxPasses int `json:"max_passes,omitempty"`
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

type stageResult struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

type drainResponse struct {
	Now       time.Time     `json:"now"`
	Revision  uint64        `json:"revision"`
	Passes    int           `json:"passes"`
	Quiescent bool          `json:"quiescent"`
	Stages    []stageResult `json:"stages"`
}

type controller struct {
	mu        sync.Mutex
	lookback  time.Duration
	maxPasses int
	timeout   time.Duration
	batchSize int
}

var address string

var Cmd = &cobra.Command{
	Use:   "simulation",
	Short: "Simulation-only control operations",
}

var ServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the simulation job drain endpoint",
	RunE: func(cmd *cobra.Command, _ []string) error {
		if os.Getenv("OPENMETER_SIMULATION_CLOCK_FILE") == "" {
			return errors.New("simulation job control requires OPENMETER_SIMULATION_CLOCK_FILE")
		}

		lookback := defaultLookback
		if value := os.Getenv("OPENMETER_SIMULATION_JOBS_LOOKBACK"); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("invalid OPENMETER_SIMULATION_JOBS_LOOKBACK %q", value)
			}
			lookback = parsed
		}

		maxPasses := defaultMaxPasses
		if value := os.Getenv("OPENMETER_SIMULATION_JOBS_MAX_PASSES"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("invalid OPENMETER_SIMULATION_JOBS_MAX_PASSES %q", value)
			}
			maxPasses = parsed
		}

		timeout := defaultTimeout
		if value := os.Getenv("OPENMETER_SIMULATION_JOBS_TIMEOUT"); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("invalid OPENMETER_SIMULATION_JOBS_TIMEOUT %q", value)
			}
			timeout = parsed
		}

		batchSize := defaultBatchSize
		if value := os.Getenv("OPENMETER_SIMULATION_JOBS_BATCH_SIZE"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("invalid OPENMETER_SIMULATION_JOBS_BATCH_SIZE %q", value)
			}
			batchSize = parsed
		}

		if address == "" {
			address = os.Getenv("OPENMETER_SIMULATION_JOBS_ADDRESS")
		}
		if address == "" {
			address = defaultAddress
		}

		controller := &controller{
			lookback:  lookback,
			maxPasses: maxPasses,
			timeout:   timeout,
			batchSize: batchSize,
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		})
		mux.HandleFunc("/__sim/jobs/drain", controller.handleDrain)

		server := &http.Server{Addr: address, Handler: mux}
		serverErr := make(chan error, 1)
		go func() {
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- err
			}
			close(serverErr)
		}()

		select {
		case err := <-serverErr:
			if err == nil {
				return nil
			}
			return err
		case <-cmd.Context().Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return server.Shutdown(shutdownContext)
		}
	},
}

func init() {
	ServeCmd.Flags().StringVar(&address, "address", "", "HTTP listen address (defaults to OPENMETER_SIMULATION_JOBS_ADDRESS)")
	Cmd.AddCommand(ServeCmd)
}

func (c *controller) handleDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var request drainRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil && !errors.Is(err, context.Canceled) {
			// An empty body is valid; only reject malformed non-empty JSON.
			if !errors.Is(err, io.EOF) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
	}
	maxPasses := request.MaxPasses
	if maxPasses <= 0 {
		maxPasses = c.maxPasses
	}
	if maxPasses > 32 {
		maxPasses = 32
	}
	timeout := c.timeout
	if request.TimeoutMS > 0 {
		timeout = time.Duration(request.TimeoutMS) * time.Millisecond
	}
	if timeout <= 0 || timeout > 60*time.Second {
		timeout = c.timeout
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot, err := clock.GetSimulationSnapshot()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	response := drainResponse{
		Now:      snapshot.Now,
		Revision: snapshot.Revision,
		Stages:   make([]stageResult, 0, maxPasses*4),
	}
	for pass := 1; pass <= maxPasses; pass++ {
		response.Passes = pass
		start := len(response.Stages)
		response.Stages = append(response.Stages, c.runPass(r.Context(), timeout)...)
		passErrors := response.Stages[start:]
		if len(passErrors) > 0 && allStagesSucceeded(passErrors) {
			response.Quiescent = true
			break
		}
		if !allStagesSucceeded(passErrors) {
			break
		}
	}
	if response.Passes == 0 {
		response.Quiescent = true
	}
	// A stage error is a product finding, not a control-plane transport
	// failure. Return it as structured JSON so the runner can continue and
	// include the finding in the final report.
	writeJSON(w, http.StatusOK, response)
}

func (c *controller) runPass(ctx context.Context, timeout time.Duration) []stageResult {
	results := make([]stageResult, 0, 4)
	appendStage := func(name string, fn func(context.Context) error) bool {
		result := stageResult{Name: name}
		stageContext, cancel := context.WithTimeout(ctx, timeout)
		err := fn(stageContext)
		cancel()
		if err != nil {
			result.Error = err.Error()
		}
		results = append(results, result)
		return result.Error == ""
	}

	if !appendStage("subscription_reconciliation", func(stageContext context.Context) error {
		return internal.App.BillingSubscriptionReconciler.All(stageContext, reconciler.ReconcilerAllInput{
			ReconcilerListSubscriptionsInput: reconciler.ReconcilerListSubscriptionsInput{
				Namespaces: []string{"default"},
				Lookback:   c.lookback,
			},
			Force: false,
		})
	}) {
		return results
	}
	if internal.App.ChargesAutoAdvancer != nil {
		if !appendStage("charge_advancement", func(stageContext context.Context) error {
			return internal.App.ChargesAutoAdvancer.All(stageContext, []string{"default"})
		}) {
			return results
		}
	}
	if !appendStage("invoice_collection", func(stageContext context.Context) error {
		return internal.App.BillingCollector.All(stageContext, []string{"default"}, nil, c.batchSize)
	}) {
		return results
	}
	appendStage("invoice_advancement", func(stageContext context.Context) error {
		return internal.App.BillingAutoAdvancer.All(stageContext, []string{"default"}, c.batchSize)
	})
	return results
}

func allStagesSucceeded(stages []stageResult) bool {
	if len(stages) == 0 {
		return true
	}
	for _, stage := range stages {
		if stage.Error != "" {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/container"
	"github.com/nektos/act/pkg/model"
)

// Runner provides capabilities to run GitHub actions
type Runner interface {
	NewPlanExecutor(plan *model.Plan) common.Executor
}

// Config contains the config for a new runner
type Config struct {
	Actor                              string                     // the user that triggered the event
	Workdir                            string                     // path to working directory
	BindWorkdir                        bool                       // bind the workdir to the job container
	EventName                          string                     // name of event to run
	EventPath                          string                     // path to JSON file to use for event.json in containers
	DefaultBranch                      string                     // name of the main branch for this repository
	ReuseContainers                    bool                       // reuse containers to maintain state
	ForcePull                          bool                       // force pulling of the image, even if already present
	ForceRebuild                       bool                       // force rebuilding local docker image action
	LogOutput                          bool                       // log the output from docker run
	JSONLogger                         bool                       // use json or text logger
	Env                                map[string]string          // env for containers
	Inputs                             map[string]string          // manually passed action inputs
	Secrets                            map[string]string          // list of secrets
	Token                              string                     // GitHub token
	InsecureSecrets                    bool                       // switch hiding output when printing to terminal
	Platforms                          map[string]string          // list of platforms
	Privileged                         bool                       // use privileged mode
	UsernsMode                         string                     // user namespace to use
	ContainerArchitecture              string                     // Desired OS/architecture platform for running containers
	ContainerDaemonSocket              string                     // Path to Docker daemon socket
	ContainerOptions                   string                     // Options for the job container
	UseGitIgnore                       bool                       // controls if paths in .gitignore should not be copied into container, default true
	GitHubInstance                     string                     // GitHub instance to use, default "github.com"
	ContainerCapAdd                    []string                   // list of kernel capabilities to add to the containers
	ContainerCapDrop                   []string                   // list of kernel capabilities to remove from the containers
	AutoRemove                         bool                       // controls if the container is automatically removed upon workflow completion
	ArtifactServerPath                 string                     // the path where the artifact server stores uploads
	ArtifactServerAddr                 string                     // the address the artifact server binds to
	ArtifactServerPort                 string                     // the port the artifact server binds to
	NoSkipCheckout                     bool                       // do not skip actions/checkout
	RemoteName                         string                     // remote name in local git repo config
	ReplaceGheActionWithGithubCom      []string                   // Use actions from GitHub Enterprise instance to GitHub
	ReplaceGheActionTokenWithGithubCom string                     // Token of private action repo on GitHub.
	Matrix                             map[string]map[string]bool // Matrix config to run

	PresetGitHubContext   *model.GithubContext         // the preset github context, overrides some fields like DefaultBranch, Env, Secrets etc.
	EventJSON             string                       // the content of JSON file to use for event.json in containers, overrides EventPath
	ContainerNamePrefix   string                       // the prefix of container name
	ContainerMaxLifetime  time.Duration                // the max lifetime of job containers
	ContainerNetworkMode  string                       // the network mode of job containers
	DefaultActionInstance string                       // the default actions web site
	PlatformPicker        func(labels []string) string // platform picker, it will take precedence over Platforms if isn't nil
	JobLoggerLevel        *log.Level                   // the level of job logger
	Vars                  map[string]string            // the list of variables set at the repository, environment, or organization levels.
}

// GetToken: Adapt to Gitea
func (c Config) GetToken() string {
	token := c.Secrets["GITHUB_TOKEN"]
	if c.Secrets["GITEA_TOKEN"] != "" {
		token = c.Secrets["GITEA_TOKEN"]
	}
	return token
}

func (c Config) IsNetworkModeHost() bool {
	return c.ContainerNetworkMode == "host"
}

func (c Config) IsNetworkModeNone() bool {
	return c.ContainerNetworkMode == "none"
}

func (c Config) IsNetworkModeBridge() bool {
	return c.ContainerNetworkMode == "bridge"
}

func (c Config) IsNetworkModeContainer() bool {
	parts := strings.SplitN(string(c.ContainerNetworkMode), ":", 2)
	return len(parts) > 1 && parts[0] == "container"
}

func (c Config) IsNetworkUserDefined() bool {
	return !c.IsNetworkModeHost() && !c.IsNetworkModeNone() && !c.IsNetworkModeBridge() && !c.IsNetworkModeContainer()
}

type caller struct {
	runContext *RunContext
}

type runnerImpl struct {
	config    *Config
	eventJSON string
	caller    *caller // the job calling this runner (caller of a reusable workflow)
}

// New Creates a new Runner
func New(runnerConfig *Config) (Runner, error) {
	runner := &runnerImpl{
		config: runnerConfig,
	}

	return runner.configure()
}

func (runner *runnerImpl) configure() (Runner, error) {
	runner.eventJSON = "{}"
	if runner.config.EventJSON != "" {
		runner.eventJSON = runner.config.EventJSON
	} else if runner.config.EventPath != "" {
		log.Debugf("Reading event.json from %s", runner.config.EventPath)
		eventJSONBytes, err := os.ReadFile(runner.config.EventPath)
		if err != nil {
			return nil, err
		}
		runner.eventJSON = string(eventJSONBytes)
	} else if len(runner.config.Inputs) != 0 {
		eventMap := map[string]map[string]string{
			"inputs": runner.config.Inputs,
		}
		eventJSON, err := json.Marshal(eventMap)
		if err != nil {
			return nil, err
		}
		runner.eventJSON = string(eventJSON)
	}
	return runner, nil
}

// NewPlanExecutor ...
func (runner *runnerImpl) NewPlanExecutor(plan *model.Plan) common.Executor {
	maxJobNameLen := 0

	stagePipeline := make([]common.Executor, 0)
	for i := range plan.Stages {
		stage := plan.Stages[i]
		stagePipeline = append(stagePipeline, func(ctx context.Context) error {
			pipeline := make([]common.Executor, 0)
			for _, run := range stage.Runs {
				stageExecutor := make([]common.Executor, 0)
				job := run.Job()

				if job.Strategy != nil {
					strategyRc := runner.newRunContext(ctx, run, nil)
					if err := strategyRc.NewExpressionEvaluator(ctx).EvaluateYamlNode(ctx, &job.Strategy.RawMatrix); err != nil {
						log.Errorf("Error while evaluating matrix: %v", err)
					}
				}

				var matrixes []map[string]interface{}
				if m, err := job.GetMatrixes(); err != nil {
					log.Errorf("Error while get job's matrix: %v", err)
				} else {
					matrixes = selectMatrixes(m, runner.config.Matrix)
				}
				log.Debugf("Final matrix after applying user inclusions '%v'", matrixes)

				maxParallel := 4
				if job.Strategy != nil {
					maxParallel = job.Strategy.MaxParallel
				}

				if len(matrixes) < maxParallel {
					maxParallel = len(matrixes)
				}

				for i, matrix := range matrixes {
					matrix := matrix
					rc := runner.newRunContext(ctx, run, matrix)
					rc.JobName = rc.Name
					if len(matrixes) > 1 {
						rc.Name = fmt.Sprintf("%s-%d", rc.Name, i+1)
					}
					if len(rc.String()) > maxJobNameLen {
						maxJobNameLen = len(rc.String())
					}
					stageExecutor = append(stageExecutor, func(ctx context.Context) error {
						jobName := fmt.Sprintf("%-*s", maxJobNameLen, rc.String())
						return rc.Executor()(common.WithJobErrorContainer(WithJobLogger(ctx, rc.Run.JobID, jobName, rc.Config, &rc.Masks, matrix)))
					})
				}
				pipeline = append(pipeline, common.NewParallelExecutor(maxParallel, stageExecutor...))
			}
			var ncpu int
			info, err := container.GetHostInfo(ctx)
			if err != nil {
				log.Errorf("failed to obtain container engine info: %s", err)
				ncpu = 1 // sane default?
			} else {
				ncpu = info.NCPU
			}
			return common.NewParallelExecutor(ncpu, pipeline...)(ctx)
		})
	}

	return common.NewPipelineExecutor(stagePipeline...).Then(handleFailure(plan))
}

func handleFailure(plan *model.Plan) common.Executor {
	return func(ctx context.Context) error {
		for _, stage := range plan.Stages {
			for _, run := range stage.Runs {
				if run.Job().Result == "failure" {
					return fmt.Errorf("Job '%s' failed", run.String())
				}
			}
		}
		return nil
	}
}

func selectMatrixes(originalMatrixes []map[string]interface{}, targetMatrixValues map[string]map[string]bool) []map[string]interface{} {
	matrixes := make([]map[string]interface{}, 0)
	for _, original := range originalMatrixes {
		flag := true
		for key, val := range original {
			if allowedVals, ok := targetMatrixValues[key]; ok {
				valToString := fmt.Sprintf("%v", val)
				if _, ok := allowedVals[valToString]; !ok {
					flag = false
				}
			}
		}
		if flag {
			matrixes = append(matrixes, original)
		}
	}
	return matrixes
}

func (runner *runnerImpl) newRunContext(ctx context.Context, run *model.Run, matrix map[string]interface{}) *RunContext {
	rc := &RunContext{
		Config:      runner.config,
		Run:         run,
		EventJSON:   runner.eventJSON,
		StepResults: make(map[string]*model.StepResult),
		Matrix:      matrix,
		caller:      runner.caller,
	}
	rc.ExprEval = rc.NewExpressionEvaluator(ctx)
	rc.Name = rc.ExprEval.Interpolate(ctx, run.String())

	return rc
}

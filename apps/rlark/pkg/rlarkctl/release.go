package rlarkctl

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"

	"github.com/rlinf/rlark/apps/rlark/pkg/release"
)

// ReleaseCommand is what a person rolling out a batch runs between its
// stages: a baseline before the batch, the checks of a stage after it. It
// reads the management plane (--kubeconfig) and changes nothing.
func ReleaseCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "The stages of a release batch and the checks between them.",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "plan",
		Short: "Print the stages in the order they are rolled out, with their gates and checks.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stages, err := release.LoadStages(release.StagesYAML)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for i, stage := range stages {
				_, _ = fmt.Fprintf(out, "%d. %s", i+1, stage.Name)
				if len(stage.Requires) > 0 {
					_, _ = fmt.Fprintf(out, "  (after %s)", strings.Join(stage.Requires, ", "))
				}
				_, _ = fmt.Fprintln(out)
				for _, change := range stage.Changes {
					_, _ = fmt.Fprintf(out, "     changes  %s\n", change)
				}
				for _, gate := range stage.Gate {
					_, _ = fmt.Fprintf(out, "     gate     %s\n", describe(gate))
				}
				for _, check := range stage.Check {
					_, _ = fmt.Fprintf(out, "     check    %s\n", describe(check))
				}
				if stage.Note != "" {
					_, _ = fmt.Fprintf(out, "     note     %s\n", stage.Note)
				}
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "snapshot",
		Short: "Print what the checks read of the management plane, as the baseline of a batch.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			snapshot, err := readSnapshot(cmd)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			// Pairs without a path read "a -> b".
			encoder.SetEscapeHTML(false)
			encoder.SetIndent("", "  ")
			return encoder.Encode(snapshot)
		},
	})

	var baselinePath, stageName string
	var checkNames []string
	var allow release.Allow
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Run checks against a baseline: those of a stage, the named ones, or all of them.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var conditions []release.Condition
			switch {
			case stageName != "":
				stages, err := release.LoadStages(release.StagesYAML)
				if err != nil {
					return err
				}
				at := slices.IndexFunc(stages, func(stage release.Stage) bool { return stage.Name == stageName })
				if at < 0 {
					return fmt.Errorf("%s is not a stage", stageName)
				}
				conditions = stages[at].Check
			case len(checkNames) > 0:
				for _, name := range checkNames {
					if _, known := release.Checks[name]; !known {
						return fmt.Errorf("%s is not a check", name)
					}
					conditions = append(conditions, release.Condition{Check: name})
				}
			default:
				for name := range release.Checks {
					conditions = append(conditions, release.Condition{Check: name})
				}
				slices.SortFunc(conditions, func(a, b release.Condition) int { return strings.Compare(a.Check, b.Check) })
			}

			data, err := os.ReadFile(baselinePath)
			if err != nil {
				return err
			}
			var baseline release.Snapshot
			if err := json.Unmarshal(data, &baseline); err != nil {
				return fmt.Errorf("%s: %w", baselinePath, err)
			}
			current, err := readSnapshot(cmd)
			if err != nil {
				return err
			}
			// From here on a failure is a reading, not a misuse.
			cmd.SilenceUsage = true
			return verifyConditions(cmd.OutOrStdout(), conditions, baseline, current, allow)
		},
	}
	verify.Flags().StringVar(&baselinePath, "baseline", "", "The snapshot taken before the batch")
	verify.Flags().StringVar(&stageName, "stage", "", "Run the checks of this stage")
	verify.Flags().StringSliceVar(&checkNames, "checks", nil, "Run these checks")
	verify.Flags().StringArrayVar(&allow.Documents.IDs, "allow-document", nil, "A member (<clusterID>/<node>) whose document the stage changes")
	verify.Flags().BoolVar(&allow.Documents.All, "allow-all-documents", false, "The stage changes every member's document")
	verify.Flags().StringArrayVar(&allow.AddonVersions.IDs, "allow-addon-version", nil, "An Addon (<clusterID>/<addon>) whose version the stage changes")
	verify.Flags().BoolVar(&allow.AddonVersions.All, "allow-all-addon-versions", false, "The stage changes Addon versions")
	_ = verify.MarkFlagRequired("baseline")
	verify.MarkFlagsMutuallyExclusive("stage", "checks")
	cmd.AddCommand(verify)

	return cmd
}

func describe(condition release.Condition) string {
	if condition.Manual != "" {
		return "manual: " + condition.Manual
	}
	return condition.Check
}

func readSnapshot(cmd *cobra.Command) (release.Snapshot, error) {
	config, err := KubeClientConfig.BuildRestConfig()
	if err != nil {
		return release.Snapshot{}, err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return release.Snapshot{}, err
	}
	return release.Read(cmd.Context(), client, time.Now())
}

// verifyConditions prints one line per condition. A manual one is printed
// for the person to confirm and fails nothing.
func verifyConditions(out io.Writer, conditions []release.Condition, baseline, current release.Snapshot, allow release.Allow) error {
	failed := 0
	for _, condition := range conditions {
		if condition.Manual != "" {
			_, _ = fmt.Fprintf(out, "manual  %s\n", condition.Manual)
			continue
		}
		result := release.Checks[condition.Check](baseline, current, allow)
		verdict := "ok"
		if !result.OK {
			verdict = "FAILED"
			failed++
		}
		_, _ = fmt.Fprintf(out, "%-6s  %-20s  %s\n", verdict, condition.Check, result.Reading)
	}
	if failed > 0 {
		return fmt.Errorf("%d checks failed against the baseline of %s", failed, baseline.TakenAt.Format(time.RFC3339))
	}
	return nil
}

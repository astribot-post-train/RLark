import type { FormEvent } from "react";
import type { Detail, JobSummary, RawJobRequest } from "./api";
import { StatusDot, type Tone } from "./Charts";

type JobLabels = Pick<
  Record<string, string>,
  | "jobsTitle"
  | "submitJob"
  | "jobHint"
  | "retainedJob"
  | "environment"
  | "argv"
  | "directory"
  | "env"
  | "retryJob"
  | "loading"
  | "unavailable"
  | "noJobs"
  | "invalid"
  | "unobserved"
  | "suspended"
  | "active"
  | "suspend"
  | "jobName"
  | "phase"
  | "actions"
>;

function jobTone(job: JobSummary): Tone {
  if (!job.bindingValid || ["Failed", "Stopped"].includes(job.phase))
    return "bad";
  if (job.phase === "Succeeded") return "good";
  if (job.phase === "Running") return "info";
  return "neutral";
}

// Presentation only; the parent retains uncertain intent across refreshes and
// locks cluster navigation. Requests and authorization remain in the API layer.
export function JobsPanel({
  detail,
  t,
  writing,
  jobIntent,
  jobsLoading,
  jobs,
  onOpen,
  onSuspend,
}: {
  detail: Detail;
  t: JobLabels;
  writing: boolean;
  jobIntent?: RawJobRequest;
  jobsLoading: boolean;
  jobs?: JobSummary[];
  onOpen: () => void;
  onSuspend: (job: JobSummary) => void;
}) {
  if (!detail.jobSubmissionEnabled && !jobIntent) return null;
  const canSubmit =
    !writing &&
    detail.jobSubmissionEnabled &&
    detail.cluster.runtime === "Raw" &&
    detail.cluster.bindingValid &&
    !detail.cluster.deleting;
  return (
    <>
      <div className="tab-toolbar">
        {jobIntent && (
          <p role="status" className="notice">
            {t.retainedJob} <code>{jobIntent.operationID}</code>
          </p>
        )}
        <button
          type="button"
          className="primary"
          disabled={!canSubmit}
          onClick={onOpen}
        >
          ＋ {jobIntent ? t.retryJob : t.submitJob}
        </button>
      </div>
      {jobsLoading ? (
        <p className="muted table-empty">{t.loading}</p>
      ) : jobs === undefined ? (
        <p className="muted table-empty">{t.unavailable}</p>
      ) : !jobs.length ? (
        <p className="muted table-empty">{t.noJobs}</p>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>{t.jobName}</th>
                <th>{t.phase}</th>
                <th>{t.suspend}</th>
                <th className="col-actions">{t.actions}</th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((job) => (
                <tr key={job.bindingUID || job.name}>
                  <td>
                    <strong title={job.operationID}>{job.name}</strong>
                  </td>
                  <td>
                    <StatusDot
                      label={job.bindingValid ? job.phase || "Pending" : t.invalid}
                      tone={jobTone(job)}
                      detail={job.observed ? undefined : t.unobserved}
                    />
                  </td>
                  <td>{job.suspended ? t.suspended : t.active}</td>
                  <td className="col-actions">
                    <button
                      type="button"
                      disabled={
                        writing ||
                        job.suspended ||
                        !job.bindingValid ||
                        ["Succeeded", "Failed", "Stopped"].includes(job.phase)
                      }
                      onClick={() => onSuspend(job)}
                    >
                      {t.suspend}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

// Submission form rendered inside the parent's drawer. Field names and
// defaults are unchanged so `rawJobIntent` parsing stays the single source.
export function JobForm({
  detail,
  t,
  writing,
  jobIntent,
  onSubmit,
}: {
  detail: Detail;
  t: JobLabels;
  writing: boolean;
  jobIntent?: RawJobRequest;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
}) {
  const locked = !!jobIntent || writing;
  return (
    <form className="create" onSubmit={onSubmit}>
      {jobIntent && (
        <p role="status" className="notice">
          {t.retainedJob} <code>{jobIntent.operationID}</code>
        </p>
      )}
      <label>
        {t.environment}
        <input
          name="environment"
          required
          disabled={locked}
          defaultValue={
            jobIntent?.environment ||
            detail.nodes.find((n) => n.environment)?.environment ||
            ""
          }
        />
      </label>
      <label>
        {t.argv}
        <textarea
          name="argv"
          required
          rows={3}
          disabled={locked}
          defaultValue={JSON.stringify(
            jobIntent?.process.argv || ["/usr/bin/python3", "train.py"],
            null,
            2,
          )}
        />
      </label>
      <label>
        {t.directory}
        <input
          name="directory"
          required
          disabled={locked}
          defaultValue={jobIntent?.process.directory || "/work"}
        />
      </label>
      <label>
        {t.env}
        <textarea
          name="env"
          rows={2}
          disabled={locked}
          defaultValue={JSON.stringify(jobIntent?.process.env || [], null, 2)}
        />
      </label>
      <button
        className="primary"
        disabled={
          writing ||
          !detail.jobSubmissionEnabled ||
          (!!jobIntent && jobIntent.clusterUID !== detail.cluster.uid)
        }
      >
        {jobIntent ? t.retryJob : t.submitJob}
      </button>
    </form>
  );
}

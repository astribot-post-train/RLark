import type { FormEvent, ReactNode } from "react";
import type { Detail, JobSummary, RawJobRequest } from "./api";

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
>;

// Presentation only; the parent retains uncertain intent across refreshes and
// locks cluster navigation. Requests and authorization remain in the API layer.
export function JobsPanel({
  detail,
  t,
  writing,
  showJob,
  jobIntent,
  jobsLoading,
  jobs,
  onToggle,
  onSubmit,
  onSuspend,
}: {
  detail: Detail;
  t: JobLabels;
  writing: boolean;
  showJob: boolean;
  jobIntent?: RawJobRequest;
  jobsLoading: boolean;
  jobs?: JobSummary[];
  onToggle: () => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  onSuspend: (job: JobSummary) => void;
}) {
  return (
    (detail.jobSubmissionEnabled || jobIntent) && (
      <section className="panel">
        <div className="actions">
          <h3>{t.jobsTitle}</h3>
          <button
            disabled={
              writing ||
              !detail.jobSubmissionEnabled ||
              detail.cluster.runtime !== "Raw" ||
              !detail.cluster.bindingValid ||
              detail.cluster.deleting
            }
            onClick={onToggle}
          >
            {t.submitJob}
          </button>
        </div>
        <p className="muted">{t.jobHint}</p>
        {jobIntent && (
          <p role="status">
            {t.retainedJob} <code>{jobIntent.operationID}</code>
          </p>
        )}
        {showJob && (
          <form className="create" onSubmit={onSubmit}>
            <label>
              {t.environment}
              <input
                name="environment"
                required
                disabled={!!jobIntent || writing}
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
                disabled={!!jobIntent || writing}
                defaultValue={JSON.stringify(
                  jobIntent?.process.argv || ["/usr/bin/python3", "train.py"],
                  null,
                  2,
                )}
                rows={3}
              />
            </label>
            <label>
              {t.directory}
              <input
                name="directory"
                required
                disabled={!!jobIntent || writing}
                defaultValue={jobIntent?.process.directory || "/work"}
              />
            </label>
            <label>
              {t.env}
              <textarea
                name="env"
                disabled={!!jobIntent || writing}
                defaultValue={JSON.stringify(
                  jobIntent?.process.env || [],
                  null,
                  2,
                )}
                rows={2}
              />
            </label>
            {jobIntent && <code>{jobIntent.operationID}</code>}
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
        )}
        {jobsLoading ? (
          <p className="muted">{t.loading}</p>
        ) : jobs === undefined ? (
          <p className="muted">{t.unavailable}</p>
        ) : !jobs.length ? (
          <p className="muted">{t.noJobs}</p>
        ) : (
          <JobTable
            empty={t.noJobs}
            rows={jobs.map((job) => (
              <tr key={job.bindingUID || job.name}>
                <td>
                  <strong>{job.name}</strong>
                  <small>{job.operationID}</small>
                </td>
                <td>
                  {job.bindingValid ? job.phase || "Pending" : t.invalid}
                  {!job.observed && <small>{t.unobserved}</small>}
                </td>
                <td>{job.suspended ? t.suspended : t.active}</td>
                <td>
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
          />
        )}
      </section>
    )
  );
}
function JobTable({ empty, rows }: { empty: string; rows: ReactNode[] }) {
  return rows.length ? (
    <div className="table-scroll">
      <table>
        <tbody>{rows}</tbody>
      </table>
    </div>
  ) : (
    <p className="muted">{empty}</p>
  );
}

"""Apache Airflow DAG showing how to call `agentctl run` as a task primitive.

Pattern
-------
1. A BashOperator invokes `agentctl run`, passing the input text as an
   ENVIRONMENT VARIABLE (NOT spliced into the shell command) and writing
   the typed result to a path on a SHARED VOLUME, keyed by the DagRun's
   run_id. Multi-worker executors (Celery, Kubernetes) schedule tasks
   on different machines, so a local `/tmp` write is invisible to the
   downstream reader. The shared volume requirement is the operator's
   responsibility (mount an NFS/EFS volume at `/mnt/airflow-shared` on
   every worker), and the example bakes the path in to make the
   constraint explicit.
2. A PythonOperator reads the result JSON via `templates_dict` (so the
   `{{ dag_run.run_id }}` substitution actually fires) and pushes the
   parsed object to XCom for downstream tasks.
3. With `spec.outputSchema` set, the result file is GUARANTEED to be
   a JSON object matching the declared shape — or `agentctl run`
   exited non-zero and Airflow marked the task failed before the
   reader task even ran.

Three specific concerns the patterns below address (codex pass 1 + 2
of slice 7.3 caught all three):

  - **Shell injection.** Splicing ANY scheduler-provided value (the input
    text `{{ params.text }}`, or the result path's `{{ dag_run.run_id }}`)
    into the BashOperator's `bash_command` — even via Jinja's `tojson` or
    single quotes — is unsafe: command substitutions like `$(...)` and
    backticks stay active inside double-quoted words, and a value with a
    stray quote can break out of a single-quoted word. Bash sees all this
    BEFORE agentctl does. Solution: pass every such value via the
    operator's `env=` dict; child processes inherit env vars without going
    through the shell parser, and bash's `"$TEXT_INPUT"` / `"$RESULT_PATH"`
    are safe variable reads. Only operator-controlled CONSTANTS
    (`SHARED_DIR`, `AGENT_SPEC`) are spliced into the command directly.
  - **Template rendering in Python callables.** Only operator
    template_fields are rendered by Airflow's Jinja layer. A module-level
    string `RESULT_PATH = "/tmp/{{ dag_run.run_id }}-..."` is NOT
    rendered, and calling `.format()` on it just collapses `{{ ... }}`
    to literal `{ ... }`. Solution: pass the path via the operator's
    `templates_dict=` so Airflow renders it, then read the rendered
    value from the context inside the callable.
  - **Lost worker environment.** Passing `env=` on BashOperator WITHOUT
    `append_env=True` replaces the worker environment entirely, dropping
    `PATH`, `ANTHROPIC_API_KEY`, and anything else `agentctl` needs.
    Solution: set `append_env=True` so the custom env merges on top of
    the inherited worker env.

This file is an EXAMPLE — a real deployment would:
  - Use an Airflow Variable / Connection for the agent spec path.
  - Use KubernetesPodOperator instead of BashOperator if Airflow workers
    shouldn't run agentctl directly.
  - Add `retries` + a sensible `execution_timeout` on the classify task.
  - Choose `SHARED_DIR` based on your infrastructure (EFS, NFS, GCS Fuse,
    etc.) and validate that EVERY worker pool can read AND write that
    mount point.
"""

from __future__ import annotations

import hashlib
import json
from datetime import datetime

from airflow import DAG
from airflow.operators.bash import BashOperator
from airflow.operators.python import PythonOperator

AGENT_SPEC = "/opt/agents/text-classifier.yaml"
# A SHARED volume mounted at this path on every Airflow worker. The
# downstream `read_result` task may run on a different worker than the
# upstream `classify` task (CeleryExecutor, KubernetesExecutor), so a
# local `/tmp` path won't work. A hash of the DagRun's run_id keys the
# file to prevent concurrent runs from clobbering each other.
SHARED_DIR = "/mnt/airflow-shared/agentctl"


def _safe_key(run_id: str) -> str:
    """Derive a filesystem-safe, fixed-length basename from a DagRun run_id.

    A raw `dag_run.run_id` must NOT be used directly as a path component:
    deployments that allow custom run IDs can produce values containing
    path separators (`/`, `..` → escape `SHARED_DIR` via traversal) or
    values long enough that `<run_id>-classify-result.json` exceeds the
    common 255-byte filename limit and the write fails before the reader
    runs. Hashing collapses any run_id to 16 hex chars — safe, bounded,
    and collision-resistant enough to key per-run result files. Codex
    pass 8 of slice 7.3 caught the raw-run-id-as-filename hazard.

    Registered as a Jinja macro via `user_defined_macros` (below) so the
    `classify` (writer) and `read_result` (reader) tasks render the SAME
    key from the same run_id.
    """
    return hashlib.sha256(run_id.encode()).hexdigest()[:16]


# `safe_key` is the user-defined macro registered on the DAG below; it
# renders identically for both the writer and the reader task.
RESULT_PATH_TMPL = SHARED_DIR + "/{{ safe_key(dag_run.run_id) }}-classify-result.json"


def _read_result(ti, templates_dict, **_):
    """Read the agent's JSON output and publish it to XCom.

    The schema is enforced by `agentctl` (slice 7.2 of v0.7), so the
    file is either a well-formed `{label, confidence}` object or the
    upstream task failed and we never get here — downstream operators
    can trust the XCom value without re-validating.

    `templates_dict` is rendered by Airflow's Jinja layer BEFORE the
    callable is invoked, so `templates_dict["result_path"]` is a
    fully-substituted path string here. This is the documented way
    to template values into a PythonOperator callable.
    """
    path = templates_dict["result_path"]
    with open(path) as fh:
        result = json.load(fh)
    ti.xcom_push(key="classification", value=result)
    return result


with DAG(
    dag_id="agentctl_text_classifier",
    description="Classify text sentiment via agent-controller CLI.",
    start_date=datetime(2026, 1, 1),
    schedule=None,
    catchup=False,
    tags=["agent-controller", "example"],
    # Expose `safe_key` to this DAG's Jinja templates so RESULT_PATH_TMPL
    # can hash the run_id into a safe basename (see _safe_key above).
    user_defined_macros={"safe_key": _safe_key},
    params={
        "text": "I absolutely love the new feature, it just works.",
    },
) as dag:
    # Pass EVERY scheduler-provided value via env (NOT via the shell
    # command body) so it reaches agentctl as a literal string instead of
    # being parsed by bash:
    #   - TEXT_INPUT  = {{ params.text }}      (could contain $(...), backticks)
    #   - RESULT_PATH = {{ dag_run.run_id }}   (could contain a quote that
    #                   breaks out of a shell word — Airflow lets some
    #                   deployments set custom DagRun IDs). Codex pass 7 of
    #                   slice 7.3 caught this: the run id was previously
    #                   rendered straight into a single-quoted argument.
    # Airflow's Jinja layer renders `env` values, so the templated path
    # still gets substituted — but the result lands in an env var, and
    # bash's double-quoted `"$RESULT_PATH"` / `"$TEXT_INPUT"` expansion
    # does NOT trigger command substitution or word-splitting.
    #
    # SHARED_DIR and AGENT_SPEC are module CONSTANTS (operator-controlled,
    # not scheduler input), so splicing them into the command is safe.
    #
    # `append_env=True` is critical: without it, BashOperator REPLACES
    # the worker environment, dropping PATH, ANTHROPIC_API_KEY, and
    # everything else agentctl needs to run. With it, our custom env
    # merges on top of the inherited worker env.
    classify = BashOperator(
        task_id="classify",
        env={
            "TEXT_INPUT": "{{ params.text }}",
            "RESULT_PATH": RESULT_PATH_TMPL,
        },
        append_env=True,
        # `set -u` so a missing env var fails loud rather than silently
        # interpolating empty string (which would hit agentctl's slice
        # 7.1 empty-after-interpolation guard, but earlier is better).
        bash_command=(
            "set -euo pipefail\n"
            f"mkdir -p '{SHARED_DIR}'\n"
            "agentctl run "
            '--input "text=$TEXT_INPUT" '
            '--output-file "$RESULT_PATH" '
            f"'{AGENT_SPEC}'"
        ),
    )

    read_result = PythonOperator(
        task_id="read_result",
        python_callable=_read_result,
        # templates_dict values ARE rendered by Airflow's Jinja layer,
        # unlike module-level strings. The callable receives the
        # rendered dict as the `templates_dict` kwarg.
        templates_dict={"result_path": RESULT_PATH_TMPL},
    )

    classify >> read_result

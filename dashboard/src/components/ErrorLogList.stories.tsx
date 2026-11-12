import type { Story } from "@ladle/react";
import ErrorLogList from "./ErrorLogList";
import type { ErrorLogEntry } from "../types";

const MONO = "'IBM Plex Mono', monospace";

const at = (minsAgo: number) =>
  new Date(Date.now() - minsAgo * 60_000).toISOString();

const LONG_BATCH_ERROR =
  "creating batch job for workpool wp-f6fc8d2c57aaacf85910: googleapi: Error 400: " +
  "Invalid value for field 'job.allocationPolicy.instances[0].policy.machineType': " +
  "'n2-highmem-2'. Machine type 'n2-highmem-2' is not available in zone " +
  "us-central1-f for the requested provisioning model SPOT; try another zone or " +
  "machine type. Details: [{'@type': 'type.googleapis.com/google.rpc.ErrorInfo', " +
  "'reason': 'RESOURCE_POOL_EXHAUSTED', 'domain': 'googleapis.com', 'metadata': " +
  "{'zone': 'us-central1-f', 'machineType': 'n2-highmem-2', 'service': " +
  "'batch.googleapis.com'}}]";

const LONG_STACK_ERROR =
  "recovering tasks for job sprinkles-wg949m5e: rpc error: code = Aborted desc = " +
  "Aborted due to transaction contention\n" +
  "  at firestore.(*Client).RunTransaction (client.go:412)\n" +
  "  at monitor.(*TaskRecovery).requeue (task_recovery.go:88)\n" +
  "  at monitor.(*TaskRecovery).Poll (task_recovery.go:51)\n" +
  "  at monitor.(*Monitor).RunMonitorLoop (monitor.go:203)\n" +
  "  at runtime.goexit (asm_arm64.s:1223)";

const LONG_SINGLE_TOKEN =
  "worker heartbeat lookup failed: " +
  "projects/depmap-portal-pipeline/databases/sprinkles/documents/Workers/".repeat(
    6,
  ) +
  "7a3bbd92-4518-464d-970a-1fcb8b847779";

const mixed: ErrorLogEntry[] = [
  {
    timestamp: at(60),
    message: "poll workpool summaries: context deadline exceeded",
  },
  { timestamp: at(55), message: "pubsub receive: connection reset" },
  { timestamp: at(48), message: LONG_BATCH_ERROR },
  { timestamp: at(45), message: "batch startup monitor: batch not found" },
  { timestamp: at(40), message: "reconcile: workpool wp-abc not found" },
  { timestamp: at(33), message: LONG_STACK_ERROR },
  { timestamp: at(30), message: "summary poll: firestore unavailable" },
  { timestamp: at(22), message: LONG_SINGLE_TOKEN },
  { timestamp: at(15), message: "pubsub receive: connection reset" },
  { timestamp: at(2), message: "provisioning: quota exceeded" },
];

const Frame = ({ children }: { children: React.ReactNode }) => (
  <div style={{ padding: "2rem", fontFamily: MONO, maxWidth: 1100 }}>
    {children}
  </div>
);

export const MixedShortAndLong: Story = () => (
  <Frame>
    <ErrorLogList errors={mixed} />
  </Frame>
);

export const OnlyShort: Story = () => (
  <Frame>
    <ErrorLogList errors={mixed.filter((e) => e.message.length < 80)} />
  </Frame>
);

export const Empty: Story = () => (
  <Frame>
    <ErrorLogList errors={[]} />
  </Frame>
);

export const Loading: Story = () => (
  <Frame>
    <ErrorLogList errors={[]} loading />
  </Frame>
);

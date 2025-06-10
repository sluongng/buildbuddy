---
slug: task-scheduling-techniques
title: "Mastering the Queue: A Deep Dive into BuildBuddy's Task Scheduler"
description: "Explore the sophisticated task scheduling techniques that power BuildBuddy, ensuring efficient, fair, and resilient distributed builds."
authors: son
date: 2025-06-10:12:00:00
image: /images/blog/task-scheduling.png
tags:
  - scheduling
  - distributed systems
  - engineering
  - architecture
  - performance
---

## Introduction

In the complex world of distributed systems, efficient task scheduling is the unsung hero. It's the invisible engine ensuring that resources are used effectively, jobs complete promptly, and the entire system remains responsive and stable. Without intelligent scheduling, even the most powerful distributed systems can falter, leading to performance bottlenecks, wasted resources, and frustrated users.

At BuildBuddy, our mission is to make developers more productive by providing fast, reliable, and scalable Bazel builds. A core component enabling this is our distributed remote execution platform, which requires sophisticated task scheduling to manage and distribute build and test actions across a dynamic fleet of executors.

This blog post peels back the layers on the task scheduling system within BuildBuddy. We'll explore the fascinating techniques and challenges involved in building a robust system designed to handle high throughput and low latency for your builds and tests, crucial for any large-scale development environment.

<!-- truncate -->

## A Bird's-Eye View: System Topology

Before we dissect specific scheduling techniques, let's trace the high-level journey of a task within the BuildBuddy ecosystem. Understanding this flow provides context for the role and importance of each scheduling component.

*(A visual diagram illustrating the flow: Client -> BuildBuddy App (ExecutionServer -> SchedulerServer) -> Executor Nodes (TaskLeaser -> Executor Core) -> BuildBuddy App (ExecutionServer for updates) would be beneficial here, but is not included in this text output.)*

1.  **Client Request:** The process begins when a client, typically a build tool like Bazel, sends an `Execute` request to the BuildBuddy application. This request asks BuildBuddy to perform a specific "action"—essentially a command with its defined inputs (like source files) and expected outputs. We'll often use "task" or "execution" interchangeably with "action" in this post.

2.  **Initial Handling (BuildBuddy App - `ExecutionServer`):**
    *   The request first arrives at the `ExecutionServer` in the BuildBuddy application. Its primary duty is to check if the result for this exact action already exists in the **Action Cache**. If a valid cached result is found, the `ExecutionServer` returns it immediately, saving valuable computation time and reducing overall build duration.
    *   If the action isn't cached, the `ExecutionServer` utilizes an **Action Merging** technique (which we'll explore in detail in "Optimizing Redundancy and Latency: Action Merging and Hedging") to check if an identical action is already being processed by another client or build step, thereby preventing redundant work and conserving resources.

3.  **Scheduling (BuildBuddy App - `SchedulerServer`):**
    *   If the action genuinely needs execution (it's not cached and not already in flight via `ActionMerger`), the `ExecutionServer` forwards the task to the `SchedulerServer`.
    *   The `SchedulerServer` is the nerve center of our scheduling logic. It enqueues the task. To enhance system responsiveness and redundancy, it typically "probes" multiple suitable Executor queues simultaneously (often three, a value configurable via `probesPerTask`). This means it attempts to place a task reservation on several Executor queues. These reservation details are persisted in a shared data store (commonly Redis), making them visible across the distributed system. This initial push helps get tasks to potentially suitable executors quickly.

4.  **Execution (Executor Node):**
    *   A fleet of distributed Executor nodes constantly polls for tasks from their local queues or receives reservations pushed by the `SchedulerServer`. An Executor picks up a task reservation, which is managed by its local `PriorityTaskScheduler` instance.
    *   **Leasing is Key:** Critically, before commencing any actual work, the Executor must acquire a **lease** for the task from the `SchedulerServer`. This is orchestrated by its `TaskLeaser` component. This step is vital: even though multiple Executors might have been probed with a reservation, the lease ensures that only *one* Executor definitively claims and executes the task. This robust mechanism prevents the same work from being performed multiple times.
    *   Once the lease is secured, the Executor's main `Executor` component takes charge:
        *   It downloads all necessary input files and dependencies from the Content Addressable Store (CAS).
        *   It executes the specified command, often within a secure, isolated container environment.
        *   It then uploads the resulting output files and logs back to the CAS.
    *   Throughout this execution process, the `Executor` continuously streams updates (such as standard output, error logs, and status changes) back to the `ExecutionServer`'s `PublishOperation` endpoint.

5.  **Result Propagation and Caching:**
    *   Clients awaiting task completion (typically connected to the `ExecutionServer` via a `WaitExecution` Remote Procedure Call - RPC) receive these real-time updates through a Publish-Subscribe (PubSub) mechanism. This allows users to monitor logs and progress transparently in the BuildBuddy UI.
    *   Once the Executor signals task completion, the `ExecutionServer` stores the final result (including output digests and exit codes) in the Action Cache. This ensures that future requests for the same action can be served swiftly from the cache, leading to significant speedups on subsequent builds.

This sequence illustrates the lifecycle of a single task, involving both server-pushed elements (like probing) and executor-driven elements (like leasing). Now, let's delve into the sophisticated scheduling techniques that ensure this process is both efficient and resilient at scale.

## Priority-Based Grouped Task Queueing

One of the foundational scheduling mechanisms BuildBuddy employs is a **Priority-Based Grouped Task Queueing** system. This approach ensures fair resource allocation across different users or organizations (groups) while also processing high-priority tasks promptly within each group.

Tasks are first categorized into groups, typically based on the authenticated user or organization ID. Each group maintains its own `taskQueue`. When the central `PriorityTaskScheduler` (part of the `SchedulerServer`) needs to select the next task for an available executor (as part of the initial probing mechanism), it doesn't just pick the highest-priority task globally. Instead, it iterates through the groups in a round-robin fashion.

For each group, the scheduler selects the highest-priority task from that group's dedicated `taskQueue`. This design ensures that even if one group submits a vast number of low-priority tasks (like integration tests), it doesn't monopolize execution resources. Other groups still receive their turn, allowing their high-priority tasks (like critical compilation steps) to be processed in a timely manner. Within each group's `taskQueue`, tasks are strictly ordered by priority, meaning a group's most critical tasks are always considered first when it's that group's turn.

The benefits of this system for multi-tenant environments are significant:

1.  **Fairness Across Groups:** Round-robin selection among groups guarantees that every group receives a fair share of processing time, preventing any single entity from starving others of resources and ensuring equitable access to the shared RBE pool.
2.  **Prevents Starvation within Groups:** Prioritizing tasks within each group ensures that a user's important tasks aren't indefinitely delayed by a flood of their own lower-priority submissions, leading to more predictable performance for critical paths.

This combination of inter-group round-robin and intra-group priority scheduling creates a balanced and responsive task execution environment, crucial for a multi-tenant system like BuildBuddy.

## Resource-Aware Scheduling and Dynamic Task Sizing

Effective scheduling extends beyond mere task prioritization and grouping; it must also be acutely aware of available executor resources and individual task requirements. BuildBuddy's `PriorityTaskScheduler` (both on the `SchedulerServer` during probing/routing and on each Executor for its local queue) incorporates sophisticated **Resource-Aware Scheduling**.

Executors in the BuildBuddy fleet regularly report their available resources. These include standard metrics like CPU cores and RAM, as well as potentially custom-defined resources specific to particular execution environments (e.g., availability of GPUs or specific software licenses). When a task is considered for scheduling, its resource requirements are meticulously checked against the available capacity of potential executors. The scheduler's `canFitTask` logic is pivotal here, ensuring an executor has sufficient free resources before a task is assigned. This prevents overloading executors, which could lead to task failures or severe performance degradation for all tasks on that machine.

Furthermore, BuildBuddy is continually enhancing resource utilization through **Dynamic Task Sizing**. Instead of depending solely on user-specified resource requests—which can sometimes be inaccurate or overly cautious—the system can learn from past executions. By measuring the actual resource consumption of similar tasks over time or by employing predictive models, the scheduler can dynamically adjust resource requests for new tasks. This allows for more efficient "packing" of tasks onto executors, maximizing throughput without compromising stability. For instance, if a specific type of compilation task consistently uses less memory than requested, future instances might be allocated a slightly smaller memory footprint, thereby freeing up resources for other tasks.

This resource-conscious approach directly translates to better infrastructure utilization, which can lead to cost savings for on-prem users and improved scalability and responsiveness for Cloud users.

## Ensuring Reliability: Task Leasing and Reconnection

In any distributed system, ensuring each task is processed reliably and "exactly once" presents a considerable challenge. Network glitches, executor restarts, or scheduler hiccups could potentially lead to tasks being dropped or, conversely, executed multiple times. BuildBuddy mitigates these risks through a robust **Task Leasing and Reconnection** mechanism. This involves the `TaskLeaser` component on the `SchedulerServer` and corresponding client-side logic within each Executor's own `TaskLeaser` instance.

When an Executor receives a task reservation (either from an initial probe or through work-stealing, which we'll discuss soon), it doesn't just start working; it must first acquire an exclusive **lease** for that task from the `SchedulerServer`. This lease is a short-term claim, signifying that the Executor has accepted responsibility and is actively working on it. The lease's core purpose is to ensure only one Executor believes it owns the task at any given time, preventing redundant work and potential conflicts.

Executors are responsible for periodically renewing their task leases with the `SchedulerServer`. If an Executor fails to renew a lease (perhaps it crashed or became disconnected), the lease eventually expires. Once expired, the `TaskLeaser` on the `SchedulerServer` can safely make the task available for rescheduling on another healthy Executor, or for acquisition via work-stealing.

A key feature enhancing fault tolerance is **lease reconnection**. If an Executor temporarily disconnects and then reconnects, its local `TaskLeaser` allows it to attempt to reclaim leases for tasks it was previously working on, provided those leases haven't already expired and been reassigned by the `SchedulerServer`. This clever feature prevents unnecessary re-computation if an Executor experiences a transient network issue but quickly recovers, allowing it to seamlessly continue its work.

The `TaskLeaser` system is central to this entire process, handling lease grants, renewals, expirations, and the reconnection logic. This system is vital for maintaining high availability and ensuring user tasks complete reliably despite the inherent uncertainties of distributed environments.

## Intelligent Task Placement: Affinity and CI Runner Routing

Beyond initial probing, wisely choosing *which* available Executor should run a task can significantly influence performance. BuildBuddy's `TaskRouter` component (part of the `SchedulerServer`) employs several strategies for intelligent task placement when deciding which executors to probe, including **Affinity Routing** and specialized **CI Runner Routing**.

**Affinity Routing** leverages the principle of "data locality" or "cache warmth." If a particular Executor has recently executed tasks similar to the current one (e.g., for the same user, project, or even a specific build target), it's likely to have relevant data already cached in its local execution root or container image layers. Routing the new task to this "warm" Executor can yield substantial performance gains by improving cache hit rates for inputs, thereby reducing the need to download dependencies or recompute intermediate steps. The `TaskRouter` factors in this affinity when ranking potential Executors for probing.

**CI Runner Routing** is a specialized form of affinity routing tailored for Continuous Integration (CI) workloads. For CI jobs, metadata like the Git repository URL and branch name are strong indicators of the required workspace context. BuildBuddy can use this information to route CI tasks to runners that have previously handled jobs for the same repository and branch. This increases the likelihood that the runner already possesses a synchronized workspace with the correct commit checked out, potentially saving considerable time otherwise spent on cloning or updating the repository. This is especially beneficial for iterative development workflows where CI jobs are frequently triggered on the same branches.

The advantages of these intelligent routing strategies are numerous:
-   **Improved Performance:** Faster task completion due to better cache utilization and reduced setup overhead.
-   **Better Resource Utilization:** Maximizing the utility of cached data translates to less network bandwidth and disk I/O consumed for fetching resources.
-   **Faster CI Builds:** Specifically for CI tasks, routing to runners with warm workspaces dramatically accelerates the feedback loop for developers using BuildBuddy Workflows or other CI systems configured with BuildBuddy RBE.

The `TaskRouter` is crucial in evaluating these and other factors to rank suitable Executors for the initial task probes, ultimately aiming to place each task on the node best equipped to execute it most efficiently.

## Proactive Task Acquisition: Work-Stealing

While the `SchedulerServer` actively pushes initial task reservations to a few selected executors (probing), BuildBuddy also incorporates a decentralized, pull-based mechanism often referred to as **Work-Stealing**. This allows idle or underutilized executors to proactively request more work, ensuring that processing capacity is rapidly matched with pending tasks. This is especially beneficial in autoscaling environments where new executors might come online without initial work, or when the initial probes don't result in an immediate lease.

**Role of the `SchedulerServer` in Work-Stealing:**

*   **Global Executor Pool Management:** The `SchedulerServer` maintains an awareness of all available executors, typically tracked in Redis.
*   **Unclaimed Task Tracking:** Tasks that are ready for execution but haven't yet been successfully leased by an executor are marked as 'unclaimed'. This is often managed using a dedicated set in Redis for each task group (e.g., `unclaimedTasks/groupID`), representing the global pool of available work.
*   **Handling `AskForMoreWorkRequest`:** Idle executors can send an `AskForMoreWorkRequest` to the `SchedulerServer`. In response, the `SchedulerServer` samples tasks from the relevant `unclaimedTasks` set in Redis. It then offers tasks that fit the requesting executor's capabilities (resource-wise) back to the executor as new reservations.

**Role of Executor Components in Work-Stealing:**

*   **`SchedulerClient` (on Executor):** This component monitors the executor's idleness. It uses heuristics like `HasExcessCapacity` (e.g., if the local queue is short or CPU utilization is below an `excess_capacity_threshold`) to determine if it should ask for more work. If it deems the executor idle, it sends the `AskForMoreWorkRequest` to the `SchedulerServer`. If no suitable work is offered, the client will typically back off for a configurable period (`idleExecutorMoreWorkTimeout`) before trying again.
*   **`PriorityTaskScheduler` (on Executor):** This component manages the executor's local task queue. When new task reservations are received (either via the initial push/probe or through work-stealing), they are added here. It de-duplicates reservations for the same task and ensures the `TaskLeaser` is invoked before actual execution.
*   **`TaskLeaser` (on Executor):** Before executing any task, the executor must successfully acquire its lease. The `TaskLeaser` on the executor communicates with the `SchedulerServer`'s `LeaseTask` RPC. This RPC internally uses a mechanism like `redisAcquireClaim` to atomically "claim" the task. A successful claim also results in the task being removed from the global `unclaimedTasks/...` set in Redis.

This executor-initiated "pull" model complements the scheduler's initial "push" model. By allowing idle executors to actively seek out work, work-stealing helps to:
*   Improve resource utilization, especially with fluctuating loads or new executors from autoscaling.
*   Reduce task latency by ensuring that as soon as an executor is free, it can quickly find and start processing available work.
*   Enhance system responsiveness and adaptability to dynamic workload changes.

## Optimizing Redundancy and Latency: Action Merging and Hedging

Two powerful techniques BuildBuddy employs—one to prevent redundant work and another to reduce tail latencies—are **Action Merging** and **Hedging**. These operate primarily at the `ExecutionServer` level, influencing tasks before they are even deeply enqueued or by managing how they are presented to the scheduling system.

### Action Merging: Avoiding Duplicate Efforts
The core problem Action Merging solves is straightforward: multiple clients or build steps might concurrently request the exact same action, identified by an identical **Action Digest** (a hash of the command, inputs, and properties). Without intervention, each request would be scheduled and executed independently, consuming valuable executor resources for redundant computations.

BuildBuddy's `ExecutionServer`, with the help of the `ActionMerger` component, tackles this head-on:
*   **Redis-Based Coordination:** The mechanism relies on Redis to track in-flight actions.
    *   When a new action request arrives, its digest is used to look up a potential canonical (first-seen) **Execution ID**.
    *   A reverse map from this Execution ID back to the Action Digest's Redis key is also maintained.
*   **Handling New Requests:**
    *   If a canonical Execution ID exists for the Action Digest, the `ActionMerger` checks if this execution is still considered active (e.g., by calling `schedulerService.ExistsTask` to see if the task is still known to the scheduler).
    *   If it's active, the new request is "merged." This might involve incrementing a counter for the canonical execution, and the new request will effectively wait for the result of this ongoing canonical execution.
    *   If no active canonical execution is found (either it never existed, or it completed/timed out), the current request initiates a *new* canonical execution. Its Execution ID is stored, and the reverse map entry is created.
*   **Lifecycle Management:** Time-to-live (TTL) values are used for these Redis entries. When an executor successfully leases the task (via `TaskLeaser.Lease`), the TTLs on the Action Merging entries are typically updated or extended to reflect that the task is now actively being processed. Upon final completion (or permanent failure) of the action, these Redis entries are deleted.

The **primary benefit** of Action Merging is substantial resource saving by preventing the system from executing the same computationally expensive task multiple times simultaneously. This is controlled by the flag `remote_execution.enable_action_merging`, which defaults to true and is essential for efficient shared RBE pools.

### Hedging: Racing for Faster Results
While Action Merging prevents unnecessary duplicate work, it can introduce a new challenge: if the single, canonical execution of an action gets stuck on a particularly slow or misbehaving executor, all merged requests waiting for its result will also be delayed. This can impact **tail latency**—the small percentage of requests that take much longer than average.

**Hedging** is an enhancement to Action Merging designed to mitigate this risk. It allows the system to proactively start additional, concurrent copies of an action if the canonical one seems to be taking too long.

*   **`shouldHedge` Logic:** The decision to hedge is made when `FindPendingExecution` (part of the `ActionMerger`'s lookup process for a canonical execution) is called. It's based on two main configuration parameters:
    *   `remote_execution.action_merging_hedge_count`: This determines the maximum number of *additional* (hedged) copies of an action that can be started. A value of 0 (the default) disables hedging.
    *   `remote_execution.action_merging_hedge_delay`: This specifies the minimum delay required after the *last* submitted execution (canonical or a previous hedge) before a new hedged copy can be initiated. This delay often incorporates a linear backoff, meaning subsequent hedges for the same action will wait progressively longer.
*   **Initiating Hedged Executions:** If `shouldHedge` evaluates to true (i.e., hedging is enabled, the max hedge count hasn't been reached, and the required delay has passed), a new execution task is initiated for the same action. An internal counter, `hedgedExecutionCount`, stored in the Redis hash for the action, is incremented. This new task then flows through the regular scheduling process.
*   **The Race to Completion:** The first execution to complete successfully (whether it's the original canonical one or any of its hedged siblings) "wins." Its result is provided to all waiting requesters. Other running copies of the same action would ideally be cancelled at this point to free up resources, though the specifics of the cancellation mechanism are beyond the scope of this post. If an execution fails permanently, other copies continue to run (up to their own retry limits).

The **primary benefit** of Hedging is that it mitigates the risk posed by slow or stuck executors, leading to more predictable and often faster overall action completion times, particularly for tasks that might otherwise fall into the tail end of the latency distribution. Hedging requires Action Merging to be enabled and is controlled by `remote_execution.action_merging_hedge_count` and `remote_execution.action_merging_hedge_delay`.

### Action Merging vs. Hedging: A Quick Summary
It's important to distinguish between these two related but distinct features:
*   **Action Merging:** Aims to *prevent unnecessary duplicate executions* of identical actions by making subsequent requests wait for the outcome of the first (canonical) one. This saves resources.
*   **Hedging:** Aims to *reduce latency for a given action* by intentionally launching limited, time-delayed duplicate executions to race against each other. This improves responsiveness at the cost of potentially running more than one copy for a brief period.

Together, these mechanisms help BuildBuddy optimize for both resource efficiency and reduced execution latency, directly impacting the speed and cost-effectiveness of remote builds.

## Fine-Tuning Performance: Advanced Scheduling Features

Beyond the core scheduling algorithms, work-stealing, and the pre-emptive optimizations of Action Merging and Hedging, BuildBuddy incorporates other advanced features to further refine performance and resource utilization.

**Queue Trimming (Post-Scheduling Optimization):** In dynamic environments, an Executor might receive a task reservation (via probing or work-stealing) for a task that has, unbeknownst to it, already been completed or is definitively leased by another executor. To prevent wasted effort and free up local capacity sooner, Executors perform **queue trimming**. The `PriorityTaskScheduler` on each Executor includes `trimQueue` logic, allowing it to verify the status of tasks in its local queue before attempting to lease or execute them. If a task is already done or irrevocably claimed elsewhere (which can be checked quickly against the `SchedulerServer`'s lease information), it's safely dropped from the local queue.

**Custom Resource Scheduling:** Modern workloads frequently demand specialized resources beyond just CPU and RAM, such as GPUs or specific hardware accelerators. BuildBuddy's scheduler accommodates tasks with such **custom resource** needs. When the `PriorityTaskScheduler`'s `getNextSchedulableTask` logic evaluates tasks, it considers these requirements. Importantly, if a high-priority task is blocked *solely* because its specific custom resource is currently unavailable (e.g., all GPUs are in use), the scheduler can intelligently skip this task for a limited period. This allows other, potentially lower-priority tasks that *don't* require the scarce custom resource to proceed on general-purpose executors, preventing "head-of-line blocking" and ensuring overall system throughput doesn't grind to a halt.

These advanced features showcase the intricate level of detail necessary for building a truly efficient and resilient task scheduling system, one capable of adapting to diverse workloads and dynamic operational conditions while maximizing the return on compute resources.

## Conclusion

As we've journeyed through BuildBuddy's task scheduling landscape, it's clear that managing tasks in a large-scale distributed system is far more complex than a simple First-In-First-Out queue. It demands a sophisticated interplay of various techniques—from server-pushed probes to executor-pulled work-stealing, group fairness, resource awareness, reliability mechanisms, and latency optimizations—all harmonized to maximize efficiency, ensure fairness, maintain reliability, and deliver a responsive experience for users.

We've seen how **Priority-Based Grouped Queueing** ensures equitable resource distribution, while **Resource-Aware Scheduling** and **Dynamic Task Sizing** optimize executor capacity. The critical **Task Leasing and Reconnection** mechanism underpins reliability, preventing duplicate work and unnecessary re-computation. **Intelligent Affinity and CI Runner Routing** accelerate execution by leveraging data locality and workspace warmth. The proactive **Work-Stealing** model allows idle executors to actively pull tasks, improving resource utilization and reducing delays. Techniques like **Action Merging and Hedging** optimize for redundancy and latency even before tasks are deeply queued. Finally, other advanced optimizations like **Queue Trimming** and intelligent **Custom Resource Handling** further fine-tune performance in dynamic environments. Each of these elements plays an indispensable role in delivering fast, reliable, and cost-effective builds.

Collectively, these strategies forge a distributed build system that is not only powerful and scalable but also remarkably resilient to fluctuating workloads and the inevitable perturbations of a distributed environment. The ultimate aim is always to complete builds and tests as quickly and reliably as possible, and sophisticated scheduling is a cornerstone of this mission.

We hope this peek into BuildBuddy's task scheduling internals has been insightful. It highlights the depth of engineering required to power a modern remote execution platform. Perhaps it will spark ideas for your own systems, or maybe you're interested in seeing these techniques in action powering your own builds. If so, we encourage you to learn more about BuildBuddy and discover how it can accelerate your development cycles!

If you have any questions or comments, or just want to chat about distributed systems, feel free to reach out on our [Slack channel](https://community.buildbuddy.io) or email us at [hello@buildbuddy.io](mailto:hello@buildbuddy.io).


---
title: "Mastering the Queue: A Deep Dive into BuildBuddy's Task Scheduling"
description: "Explore the sophisticated task scheduling techniques that power BuildBuddy, ensuring efficient, fair, and resilient distributed builds."
authors:
  - jules
date: 2024-05-16 # Date can be updated if explicitly needed
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

This blog post peels back the layers on the task scheduling system within BuildBuddy. We'll explore the fascinating techniques and challenges involved in building a robust system designed to manage and distribute tasks across a fleet of executors. Our goal is to provide both high throughput and low latency for your builds and tests, and sophisticated scheduling is key to achieving this.

## A Bird's-Eye View: System Topology

Before we dissect specific scheduling techniques, let's trace the high-level journey of a task within the BuildBuddy ecosystem. Understanding this flow provides context for the role and importance of each scheduling component.

*(A visual diagram here would be beneficial for readers, illustrating the flow: Client -> BuildBuddy App (ExecutionServer -> SchedulerServer) -> Executor Nodes (TaskLeaser -> Executor Core) -> BuildBuddy App (ExecutionServer for updates).)*

1.  **Client Request:** The process begins when a client, typically a build tool like Bazel, sends an `Execute` request to the BuildBuddy application. This request asks BuildBuddy to perform a specific "action"—essentially a command with its defined inputs (like source files) and expected outputs. We'll often use "task" or "execution" interchangeably with "action" in this post.

2.  **Initial Handling (BuildBuddy App - `ExecutionServer`):**
    *   The request first arrives at the `ExecutionServer` in the BuildBuddy application. Its primary duty is to check if the result for this exact action already exists in the **Action Cache**. If a valid cached result is found, the `ExecutionServer` returns it immediately, saving valuable computation time.
    *   If the action isn't cached, the `ExecutionServer` utilizes an **Action Merging** technique via a component named `ActionMerger`. This component, often backed by a distributed key-value store like Redis, checks if an identical action is already being processed elsewhere in the system. If so, the new request can subscribe to the result of the ongoing action, preventing redundant work. (We'll revisit Action Merging in more detail under "Fine-Tuning Performance").

3.  **Scheduling (BuildBuddy App - `SchedulerServer`):**
    *   If the action genuinely needs execution (it's not cached and not already in flight via `ActionMerger`), the `ExecutionServer` forwards the task to the `SchedulerServer`.
    *   The `SchedulerServer` is the nerve center of our scheduling logic. It enqueues the task. To enhance system responsiveness and redundancy, it typically "probes" multiple suitable Executor queues simultaneously (often three, a value configurable via `probesPerTask`). This means it attempts to place a task reservation on several Executor queues. These reservation details are persisted in a shared data store (commonly Redis), making them visible across the distributed system. This initial push helps get tasks to potentially suitable executors quickly.

4.  **Execution (Executor Node):**
    *   A fleet of distributed Executor nodes constantly polls for tasks from their local queues. An Executor picks up a task reservation, which is managed by its local `PriorityTaskScheduler` instance.
    *   **Leasing is Key:** Critically, before commencing any actual work, the Executor must acquire a **lease** for the task from the `SchedulerServer`. This is orchestrated by its `TaskLeaser` component. This step is vital: even though multiple Executors might have been probed with a reservation, the lease ensures that only *one* Executor definitively claims and executes the task. This robust mechanism prevents the same work from being performed multiple times.
    *   Once the lease is secured, the Executor's main `Executor` component takes charge:
        *   It downloads all necessary input files and dependencies.
        *   It executes the specified command, often within a secure, isolated container environment.
        *   It then uploads the resulting output files and logs.
    *   Throughout this execution process, the `Executor` continuously streams updates (such as standard output, error logs, and status changes) back to the `ExecutionServer`'s `PublishOperation` endpoint.

5.  **Result Propagation and Caching:**
    *   Clients awaiting task completion (typically connected to the `ExecutionServer` via a `WaitExecution` Remote Procedure Call - RPC) receive these real-time updates through a Publish-Subscribe (PubSub) mechanism. This allows users to monitor logs and progress transparently.
    *   Once the Executor signals task completion, the `ExecutionServer` stores the final result (including output digests and exit codes) in the Action Cache. This ensures that future requests for the same action can be served swiftly from the cache.

This sequence illustrates the lifecycle of a single task, involving both server-pushed elements (like probing) and executor-driven elements (like leasing). Now, let's delve into the sophisticated scheduling techniques that ensure this process is both efficient and resilient at scale.

## Priority-Based Grouped Task Queueing

One of the foundational scheduling mechanisms BuildBuddy employs is a **Priority-Based Grouped Task Queueing** system. This approach ensures fair resource allocation across different users or organizations (groups) while also processing high-priority tasks promptly within each group.

Tasks are first categorized into groups, typically based on the authenticated user or organization ID. Each group maintains its own `taskQueue`. When the central `PriorityTaskScheduler` (part of the `SchedulerServer`) needs to select the next task for an available executor (as part of the initial probing mechanism), it doesn't just pick the highest-priority task globally. Instead, it iterates through the groups in a round-robin fashion.

For each group, the scheduler selects the highest-priority task from that group's dedicated `taskQueue`. This design ensures that even if one group submits a vast number of low-priority tasks, it doesn't monopolize execution resources. Other groups still receive their turn, allowing their high-priority tasks to be processed in a timely manner. Within each group's `taskQueue`, tasks are strictly ordered by priority, meaning a group's most critical tasks are always considered first when it's that group's turn.

The benefits of this system are significant:

1.  **Fairness Across Groups:** Round-robin selection among groups guarantees that every group receives a fair share of processing time, preventing any single entity from starving others of resources.
2.  **Prevents Starvation within Groups:** Prioritizing tasks within each group ensures that a user's important tasks aren't indefinitely delayed by a flood of their own lower-priority submissions.

This combination of inter-group round-robin and intra-group priority scheduling creates a balanced and responsive task execution environment, crucial for a multi-tenant system like BuildBuddy.

## Resource-Aware Scheduling and Dynamic Task Sizing

Effective scheduling extends beyond mere task prioritization and grouping; it must also be acutely aware of available executor resources and individual task requirements. BuildBuddy's `PriorityTaskScheduler` (both on the `SchedulerServer` during probing/routing and on each Executor for its local queue) incorporates sophisticated **Resource-Aware Scheduling**.

Executors in the BuildBuddy fleet regularly report their available resources. These include standard metrics like CPU cores and RAM, as well as potentially custom-defined resources specific to particular execution environments (e.g., availability of GPUs or specific software licenses). When a task is considered for scheduling, its resource requirements are meticulously checked against the available capacity of potential executors. The scheduler's `canFitTask` logic is pivotal here, ensuring an executor has sufficient free resources before a task is assigned. This prevents overloading executors, which could lead to task failures or severe performance degradation for all tasks on that machine.

Furthermore, BuildBuddy is continually enhancing resource utilization through **Dynamic Task Sizing**. Instead of depending solely on user-specified resource requests—which can sometimes be inaccurate or overly cautious—the system can learn from past executions. By measuring the actual resource consumption of similar tasks over time or by employing predictive models, the scheduler can dynamically adjust resource requests for new tasks. This allows for more efficient "packing" of tasks onto executors, maximizing throughput without compromising stability. For instance, if a specific type of compilation task consistently uses less memory than requested, future instances might be allocated a slightly smaller memory footprint, thereby freeing up resources for other tasks.

## Ensuring Reliability: Task Leasing and Reconnection

In any distributed system, ensuring each task is processed reliably and "exactly once" presents a considerable challenge. Network glitches, executor restarts, or scheduler hiccups could potentially lead to tasks being dropped or, conversely, executed multiple times. BuildBuddy mitigates these risks through a robust **Task Leasing and Reconnection** mechanism. This involves the `TaskLeaser` component on the `SchedulerServer` and corresponding client-side logic within each Executor's own `TaskLeaser` instance.

When an Executor receives a task reservation (either from an initial probe or through work-stealing, which we'll discuss soon), it doesn't just start working; it must first acquire an exclusive **lease** for that task from the `SchedulerServer`. This lease is a short-term claim, signifying that the Executor has accepted responsibility and is actively working on it. The lease's core purpose is to ensure only one Executor believes it owns the task at any given time, preventing redundant work and potential conflicts.

Executors are responsible for periodically renewing their task leases with the `SchedulerServer`. If an Executor fails to renew a lease (perhaps it crashed or became disconnected), the lease eventually expires. Once expired, the `TaskLeaser` on the `SchedulerServer` can safely make the task available for rescheduling on another healthy Executor, or for acquisition via work-stealing.

A key feature enhancing fault tolerance is **lease reconnection**. If an Executor temporarily disconnects and then reconnects, its local `TaskLeaser` allows it to attempt to reclaim leases for tasks it was previously working on, provided those leases haven't already expired and been reassigned by the `SchedulerServer`. This clever feature prevents unnecessary re-computation if an Executor experiences a transient network issue but quickly recovers, allowing it to seamlessly continue its work.

The `TaskLeaser` system is central to this entire process, handling lease grants, renewals, expirations, and the reconnection logic. This system is vital for maintaining high availability and ensuring user tasks complete reliably despite the inherent uncertainties of distributed environments.

## Intelligent Task Placement: Affinity and CI Runner Routing

Beyond initial probing, wisely choosing *which* available Executor should run a task can significantly influence performance. BuildBuddy's `TaskRouter` component (part of the `SchedulerServer`) employs several strategies for intelligent task placement when deciding which executors to probe, including **Affinity Routing** and specialized **CI Runner Routing**.

**Affinity Routing** leverages the principle of "data locality" or "cache warmth." If a particular Executor has recently executed tasks similar to the current one (e.g., for the same user, project, or even a specific build target), it's likely to have relevant data already cached. This cached data might include source code, build tools, or intermediate build artifacts. Routing the new task to this "warm" Executor can yield substantial performance gains by improving cache hit rates, thereby reducing the need to download dependencies or recompute intermediate steps. The `TaskRouter` factors in this affinity when ranking potential Executors for probing.

**CI Runner Routing** is a specialized form of affinity routing tailored for Continuous Integration (CI) workloads. For CI jobs, metadata like the Git repository URL and branch name are strong indicators of the required workspace context. BuildBuddy can use this information to route CI tasks to runners that have previously handled jobs for the same repository and branch. This increases the likelihood that the runner already possesses a synchronized workspace with the correct commit checked out, potentially saving considerable time otherwise spent on cloning or updating the repository. This is especially beneficial for iterative development workflows where CI jobs are frequently triggered on the same branches.

The advantages of these intelligent routing strategies are numerous:
-   **Improved Performance:** Faster task completion due to better cache utilization and reduced setup overhead.
-   **Better Resource Utilization:** Maximizing the utility of cached data translates to less network bandwidth and disk I/O consumed for fetching resources.
-   **Faster CI Builds:** Specifically for CI tasks, routing to runners with warm workspaces dramatically accelerates the feedback loop for developers.

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

**Overall Flow of Work-Stealing:**

1.  A new task is scheduled via `ExecutionServer` -> `SchedulerServer`. After initial checks, the `SchedulerServer` adds it to the global pool of `unclaimedTasks/groupID` in Redis.
2.  The `SchedulerServer` performs an initial push of reservations to a few executors (probing).
3.  An Executor, currently idle or with excess capacity (as determined by its `SchedulerClient`), sends an `AskForMoreWorkRequest`.
4.  The `SchedulerServer` offers suitable tasks from the `unclaimedTasks` pool as new reservations to the requesting Executor.
5.  The Executor adds these new task reservations to its local `PriorityTaskScheduler` queue.
6.  When ready, the Executor's `TaskLeaser` attempts to acquire the lease. If successful, the `SchedulerServer` atomically marks the task as claimed (removing it from the global unclaimed pool), and execution begins.

This executor-initiated "pull" model complements the scheduler's initial "push" model. By allowing idle executors to actively seek out work, work-stealing helps to:
*   Improve resource utilization, especially with fluctuating loads or new executors from autoscaling.
*   Reduce task latency by ensuring that as soon as an executor is free, it can quickly find and start processing available work.
*   Enhance system responsiveness and adaptability to dynamic workload changes.

## Fine-Tuning Performance: Advanced Scheduling Features

Beyond the core scheduling algorithms and work-stealing, BuildBuddy incorporates several other advanced features to further optimize performance and resource utilization.

**Action Merging (Pre-Queue Optimization Revisited):** As introduced in our topology overview, **Action Merging** is a powerful preemptive optimization. Before a task even enters the scheduling queues, the `ExecutionServer` (via `ActionMerger`) checks for identical in-flight actions. If found, the new request subscribes to the existing result, avoiding redundant computations entirely. This is particularly effective for identical, long-running tasks submitted concurrently and significantly lessens the load on schedulers and executors by preventing these duplicates from ever needing to be scheduled.

**Queue Trimming (Post-Scheduling Optimization):** In dynamic environments, an Executor might receive a task reservation (via probing or work-stealing) for a task that has, unbeknownst to it, already been completed or is definitively leased by another executor. To prevent wasted effort, Executors perform **queue trimming**. The `PriorityTaskScheduler` on each Executor includes `trimQueue` logic, allowing it to verify the status of tasks in its local queue before attempting to lease or execute them. If a task is already done or irrevocably claimed elsewhere, it's safely dropped, freeing local capacity sooner.

**Custom Resource Scheduling:** Modern workloads frequently demand specialized resources beyond just CPU and RAM, such as GPUs or specific hardware accelerators. BuildBuddy's scheduler accommodates tasks with such **custom resource** needs. When the `PriorityTaskScheduler`'s `getNextSchedulableTask` logic evaluates tasks, it considers these requirements. Importantly, if a high-priority task is blocked *solely* because its specific custom resource is currently unavailable (e.g., all GPUs are in use), the scheduler can intelligently skip this task for a limited period. This allows other, potentially lower-priority tasks that *don't* require the scarce custom resource to proceed, preventing "head-of-line blocking" and ensuring general-purpose Executors remain productive.

These advanced features showcase the intricate level of detail necessary for building a truly efficient and resilient task scheduling system, one capable of adapting to diverse workloads and dynamic operational conditions.

## Conclusion

As we've journeyed through BuildBuddy's task scheduling landscape, it's clear that managing tasks in a large-scale distributed system is far more complex than a simple First-In-First-Out queue. It demands a sophisticated interplay of various techniques, all harmonized to maximize efficiency, ensure fairness, maintain reliability, and deliver a responsive experience for users.

We've seen how **Priority-Based Grouped Queueing** ensures equitable resource distribution, while **Resource-Aware Scheduling** and **Dynamic Task Sizing** optimize executor capacity. The critical **Task Leasing and Reconnection** mechanism underpins reliability, and **Intelligent Affinity and CI Runner Routing** accelerate execution by leveraging data locality. Furthermore, the proactive **Work-Stealing** model allows idle executors to actively pull tasks, enhancing resource utilization and responsiveness. Finally, advanced optimizations like **Action Merging**, **Queue Trimming**, and intelligent **Custom Resource Handling** fine-tune performance in dynamic environments. Each of these elements plays an indispensable role.

Collectively, these strategies forge a distributed build system that is not only powerful and scalable but also remarkably resilient to fluctuating workloads and the inevitable perturbations of a distributed environment. The ultimate aim is always to complete builds and tests as quickly and reliably as possible, and sophisticated scheduling, encompassing both server-pushed task probing and executor-pulled work-stealing, is a cornerstone of this mission.

We hope this peek into BuildBuddy's task scheduling internals has been insightful. Perhaps it will spark ideas for your own systems, or maybe you're interested in seeing these techniques in action. If so, we encourage you to learn more about BuildBuddy and discover how it can accelerate your development cycles!
---

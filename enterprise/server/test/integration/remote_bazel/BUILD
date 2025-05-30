load("@io_bazel_rules_go//go:def.bzl", "go_test")

package(default_visibility = ["//enterprise:__subpackages__"])

go_test(
    name = "remote_bazel_test",
    size = "enormous",
    srcs = ["remote_bazel_test.go"],
    exec_properties = {
        "test.include-secrets": "true",
        "test.workload-isolation-type": "firecracker",
        # Use an image with bazelisk installed
        "test.container-image": "docker://gcr.io/flame-public/rbe-ubuntu20-04-workflows@sha256:271e5e3704d861159c75b8dd6713dbe5a12272ec8ee73d17f89ed7be8026553f",
        # The tests clone git repos, so make sure they have enough resources to do so
        "test.EstimatedComputeUnits": "3",
        "test.EstimatedFreeDiskBytes": "4GB",
        "test.recycle-runner": "true",
        "test.runner-recycling-key": "remote-bazel-integration",
    },
    shard_count = 7,
    tags = [
        # Use the "docker" tag to only run this on authenticated BB workflows
        # to support runner recycling
        "docker",
        # Use the "secrets" tag to prevent building this on unauthenticated
        # builds
        "secrets",
    ],
    deps = [
        "//cli/remotebazel",
        "//cli/testutil/testcli",
        "//enterprise/server/backends/kms",
        "//enterprise/server/execution_service",
        "//enterprise/server/githubapp",
        "//enterprise/server/hostedrunner",
        "//enterprise/server/invocation_search_service",
        "//enterprise/server/secrets",
        "//enterprise/server/test/integration/remote_execution/rbetest",
        "//enterprise/server/util/keystore",
        "//enterprise/server/workflow/service",
        "//proto:api_key_go_proto",
        "//proto:buildbuddy_service_go_proto",
        "//proto:capability_go_proto",
        "//proto:context_go_proto",
        "//proto:eventlog_go_proto",
        "//proto:invocation_go_proto",
        "//proto:invocation_status_go_proto",
        "//proto:secrets_go_proto",
        "//proto:user_id_go_proto",
        "//server/backends/memory_kvstore",
        "//server/backends/repo_downloader",
        "//server/interfaces",
        "//server/tables",
        "//server/testutil/testbazel",
        "//server/testutil/testenv",
        "//server/testutil/testfs",
        "//server/testutil/testgit",
        "//server/testutil/testshell",
        "//server/util/bazel",
        "//server/util/git",
        "//server/util/log",
        "//server/util/testing/flags",
        "@com_github_google_uuid//:uuid",
        "@com_github_stretchr_testify//require",
        "@org_golang_google_grpc//metadata",
    ],
)

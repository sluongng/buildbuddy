load("@io_bazel_rules_go//go:def.bzl", "go_test")

package(default_visibility = ["//enterprise:__subpackages__"])

go_test(
    name = "graph_execute_benchmark_test",
    size = "large",
    timeout = "long",
    srcs = ["graph_execute_benchmark_test.go"],
    tags = ["manual"],
    deps = [
        "//enterprise/server/test/integration/remote_execution/rbetest",
        "//proto:graph_execution_go_proto",
        "//proto:remote_execution_go_proto",
        "//server/interfaces",
        "//server/metrics",
        "//server/testutil/testmetrics",
        "//server/util/grpc_client",
        "//server/util/grpc_server",
        "//server/util/status",
        "@org_golang_google_grpc//:grpc",
        "@org_golang_google_grpc//metadata",
    ],
)

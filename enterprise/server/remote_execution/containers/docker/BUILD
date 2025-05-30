load("@io_bazel_rules_go//go:def.bzl", "go_library", "go_test")

package(default_visibility = ["//enterprise:__subpackages__"])

go_library(
    name = "docker",
    srcs = ["docker.go"],
    importpath = "github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/containers/docker",
    deps = [
        "//enterprise/server/remote_execution/commandutil",
        "//enterprise/server/remote_execution/container",
        "//enterprise/server/remote_execution/platform",
        "//enterprise/server/util/oci",
        "//proto:remote_execution_go_proto",
        "//server/environment",
        "//server/interfaces",
        "//server/util/flag",
        "//server/util/log",
        "//server/util/random",
        "//server/util/status",
        "@com_github_docker_docker//api/types/container",
        "@com_github_docker_docker//api/types/image",
        "@com_github_docker_docker//api/types/registry",
        "@com_github_docker_docker//client",
        "@com_github_docker_docker//pkg/stdcopy",
        "@com_github_docker_go_units//:go-units",
        "@org_golang_google_grpc//codes",
        "@org_golang_google_grpc//status",
        "@org_golang_x_sync//errgroup",
    ],
)

go_test(
    name = "docker_test",
    srcs = ["docker_test.go"],
    exec_properties = {
        # TODO(https://github.com/buildbuddy-io/buildbuddy-internal/issues/1094): Require less manual configuration here.
        "test.workload-isolation-type": "firecracker",
        "test.init-dockerd": "true",
    },
    shard_count = 2,
    tags = ["docker"],
    deps = [
        ":docker",
        "//enterprise/server/remote_execution/container",
        "//enterprise/server/util/oci",
        "//proto:remote_execution_go_proto",
        "//server/interfaces",
        "//server/testutil/testauth",
        "//server/testutil/testenv",
        "//server/testutil/testfs",
        "//server/util/disk",
        "//server/util/status",
        "@com_github_docker_docker//client",
        "@com_github_stretchr_testify//assert",
        "@com_github_stretchr_testify//require",
    ],
)

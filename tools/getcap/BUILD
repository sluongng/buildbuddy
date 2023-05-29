load("@io_bazel_rules_go//go:def.bzl", "go_binary", "go_library")

go_library(
    name = "getcap_lib",
    srcs = ["main.go"],
    importpath = "github.com/buildbuddy-io/buildbuddy/tools/getcap",
    visibility = ["//visibility:private"],
    deps = [
        "//proto:remote_execution_go_proto",
        "//server/util/grpc_client",
        "//server/util/status",
        "@com_github_mattn_go_isatty//:go-isatty",
        "@org_golang_google_protobuf//encoding/protojson",
        "@org_golang_google_protobuf//proto",
    ],
)

go_binary(
    name = "getcap",
    embed = [":getcap_lib"],
    visibility = ["//visibility:public"],
)

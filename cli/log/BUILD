load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "log",
    srcs = ["log.go"],
    importpath = "github.com/buildbuddy-io/buildbuddy/cli/log",
    deps = ["//cli/terminal"],
)

package(default_visibility = ["//cli:__subpackages__"])

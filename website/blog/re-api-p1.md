---
slug: remote-apis-explained
title: Remote APIS explained
author: Son Luong Ngoc
author_title: Solution Engineer @ BuildBuddy
date: 2023-05-28:12:00:00
author_url: https://github.com/sluongng/
author_image_url: https://avatars.githubusercontent.com/u/26684313?v=4
tags: [bazel, technical]
---

# Remote Execution API explained

In a previous series in my blog [A Babel in Bazel](https;//google.com/), we have explored different ways Bazel caches build and test results locally.
In this series, we shall explore the Remote APIs specification which Bazel uses for Remote Cache and Remote Build Execution.

## Introduction

Remote Execution APIs, or [Remote APIs](https;//google.com/), is an API specification that help define how a build tool should be interacting with a remote server.

In particular, today Remote APIs mainly support 2 prime use cases:
- Caching build (and test) results on the remote server.
- Executing builds (and test) on the remote server.

The API is defined in protobuf with support for both HTTP and GRPC api.
The specification includes multiple sets of RPCs, each serve a different purpose.
From a high-level, they look something like this:

```protobuf
service Capabilities {
  rpc GetCapabilities(...)
}

service ActionCache {
  rpc GetActionResult(...)

  rpc UpdateActionResult(...)
}

service ContentAddressableStorage {
  rpc FindMissingBlobs(...)

  rpc BatchUpdateBlobs(...)

  rpc BatchReadBlobs(...)

  rpc GetTree(...)
}

service Execution {
  rpc Execute(...)

  rpc WaitExecution(...)
}
```

So how does Bazel make use of these?

## Server Capabilities

How Bazel (the client) interacts with a server is completely depending on the server capabilities.

- You could not want to send Remote Execution requests to server that would only support Remote Caching
- You would not want to send a ZSTD compressed blob to the server to cache if the server does not know how to decompressed it to validate the SHA256 correctly.

For this reason, before any remote cache and remote execution request is made, Bazel needs to check
what the server is capable of first to adjust it's usage accordingly.

This is done using the `GetCapabilities` RPC in `Capabilities` service.
All remote cache and remote execution servers, especially GRPC ones, are expected to implement this service.

```protobuf
service Capabilities {
  rpc GetCapabilities(GetCapabilitiesRequest) returns (ServerCapabilities) {}
}

message GetCapabilitiesRequest {
  string instance_name = 1;
}

message ServerCapabilities {
  CacheCapabilities cache_capabilities = 1;

  ExecutionCapabilities execution_capabilities = 2;

  build.bazel.semver.SemVer deprecated_api_version = 3;

  build.bazel.semver.SemVer low_api_version = 4;

  build.bazel.semver.SemVer high_api_version = 5;
}
```

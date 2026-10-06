#!/usr/bin/python3

import json
import os
import sys

import seccomp


def load_profile(profile_path: str) -> seccomp.SyscallFilter:
    with open(profile_path, encoding="utf-8") as profile_file:
        profile = json.load(profile_file)

    if profile.get("defaultAction") != "SCMP_ACT_ALLOW":
        raise ValueError("only SCMP_ACT_ALLOW is supported as defaultAction")

    syscall_filter = seccomp.SyscallFilter(defaction=seccomp.ALLOW)

    for rule in profile.get("syscalls", []):
        if rule.get("action") != "SCMP_ACT_ERRNO":
            raise ValueError("only SCMP_ACT_ERRNO rules are supported")

        errno_ret = int(rule.get("errnoRet", 1))
        for syscall_name in rule.get("names", []):
            syscall_filter.add_rule(seccomp.ERRNO(errno_ret), syscall_name)

    return syscall_filter


def main() -> None:
    if len(sys.argv) < 3:
        print(
            f"usage: {sys.argv[0]} PROFILE PROGRAM [ARG ...]",
            file=sys.stderr,
        )
        raise SystemExit(2)

    profile_path = sys.argv[1]
    command = sys.argv[2:]

    syscall_filter = load_profile(profile_path)
    syscall_filter.load()

    os.execvp(command[0], command)


if __name__ == "__main__":
    main()

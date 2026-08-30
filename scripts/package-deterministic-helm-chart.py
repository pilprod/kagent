#!/usr/bin/env python3

"""Create a byte-stable Helm chart archive from an already validated directory."""

from __future__ import annotations

import argparse
import gzip
import pathlib
import stat
import tarfile


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("source", type=pathlib.Path)
    parser.add_argument("output", type=pathlib.Path)
    return parser.parse_args()


def package_chart(source: pathlib.Path, output: pathlib.Path) -> None:
    source = source.resolve(strict=True)
    if not source.is_dir() or not (source / "Chart.yaml").is_file():
        raise ValueError(f"source is not a Helm chart directory: {source}")
    if (source / ".helmignore").exists():
        raise ValueError("deterministic packager does not interpret .helmignore")

    files: list[pathlib.Path] = []
    for path in source.rglob("*"):
        if path.is_symlink():
            raise ValueError(f"chart contains a symbolic link: {path}")
        if path.is_file():
            files.append(path)
        elif not path.is_dir():
            raise ValueError(f"chart contains an unsupported file type: {path}")
    files.sort(key=lambda path: path.relative_to(source).as_posix())

    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("wb") as output_file:
        with gzip.GzipFile(
            filename="", fileobj=output_file, mode="wb", compresslevel=9, mtime=0
        ) as compressed:
            with tarfile.open(
                fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT
            ) as archive:
                for path in files:
                    file_stat = path.stat()
                    relative = path.relative_to(source).as_posix()
                    info = tarfile.TarInfo(f"{source.name}/{relative}")
                    info.size = file_stat.st_size
                    info.mode = 0o755 if file_stat.st_mode & stat.S_IXUSR else 0o644
                    info.mtime = 0
                    info.uid = 0
                    info.gid = 0
                    info.uname = ""
                    info.gname = ""
                    with path.open("rb") as chart_file:
                        archive.addfile(info, chart_file)


def main() -> None:
    args = parse_args()
    package_chart(args.source, args.output)


if __name__ == "__main__":
    main()

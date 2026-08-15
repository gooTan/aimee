#!/bin/sh
set -eu

src_dir="$1"
suffix="$2"
dst_dir="$3"

[ -d "$src_dir" ] || exit 0
mkdir -p "$dst_dir/.seeded"

for shipped_file in "$src_dir"/*"$suffix"; do
    [ -e "$shipped_file" ] || continue
    base=$(basename "$shipped_file")
    dst="$dst_dir/$base"
    record="$dst_dir/.seeded/$base"
    shipped_hash=$(sha256sum "$shipped_file" | cut -d' ' -f1)

    if [ ! -f "$dst" ]; then
        cp "$shipped_file" "$dst"
        printf '%s\n' "$shipped_hash" > "$record"
        continue
    fi

    if [ -f "$record" ]; then
        disk_hash=$(sha256sum "$dst" | cut -d' ' -f1)
        if [ "$disk_hash" = "$(cat "$record")" ] && [ "$disk_hash" != "$shipped_hash" ]; then
            cp "$shipped_file" "$dst"
            printf '%s\n' "$shipped_hash" > "$record"
        fi
    elif [ "$(sha256sum "$dst" | cut -d' ' -f1)" = "$shipped_hash" ]; then
        printf '%s\n' "$shipped_hash" > "$record"
    fi
done

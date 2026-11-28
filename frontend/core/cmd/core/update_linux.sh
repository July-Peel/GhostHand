set -eu

manager=$1
core=$2
stage=$3
candidate="$stage/ghosthand-core"
backup="$stage/previous"
stopping=0
replaced=0

service_action() {
    if [ "$manager" = systemd ]; then
        systemctl "$1" ghosthand-core.service
    else
        rc-service ghosthand-core "$1"
    fi
}

finish() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$status" -ne 0 ] && [ "$stopping" -eq 1 ]; then
        if [ "$replaced" -eq 1 ]; then
            service_action stop || true
            if [ -f "$stage/ready" ]; then
                status=0
            else
                mv -f "$backup" "$core" || exit "$status"
            fi
        fi
        service_action start || exit 1
    fi
    rm -f "$candidate" "$backup" "$stage/expected" "$stage/ready" "$stage/apply.sh"
    rmdir "$stage"
    exit "$status"
}

trap finish EXIT
trap 'exit 1' HUP INT TERM
ln "$core" "$backup"
stopping=1
service_action stop
mv -f "$candidate" "$core"
replaced=1
service_action start

count=0
while [ "$count" -lt 120 ]; do
    [ ! -f "$stage/ready" ] || exit 0
    count=$((count + 1))
    sleep 1
done
[ -f "$stage/ready" ]

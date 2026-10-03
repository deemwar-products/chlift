. "$(dirname "$0")/lib.sh"; clear
run 'chlift init --cluster demo --ssh-key ~/.ssh/chlift_dev --insecure-ignore-host-key --secrets-file ./secrets.env \
  --host node-a=127.0.0.1:2221@node-a:clickhouse,keeper --host node-b=127.0.0.1:2222@node-b:clickhouse,keeper \
  --host node-c=127.0.0.1:2223@node-c:keeper --host peer=local:peerdb \
  --peerdb-dir /tmp/chlift-demo/peerdb --peerdb-network chlift-dev_default --s3-endpoint http://chlift-s3:8333 \
  --pg-peer-host postgres --pg-peer-port 5432'
run 'chlift plan'
run 'time chlift install'
hold

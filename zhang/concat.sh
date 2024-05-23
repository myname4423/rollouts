DIR="/Users/boyun/Downloads/kruise-rollout"
find "$DIR" -type f ! -name '*test.go' ! -name 'concatenated_output.go' | while read -r file; do
    echo "==> $file <=="
    cat "$file"
done > concatenated_output.go

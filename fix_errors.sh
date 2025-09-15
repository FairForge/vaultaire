#!/bin/bash

# Add context to bare error returns
files=$(grep -r "return err" internal/ | grep -v "fmt.Errorf" | grep -v "_test.go" | cut -d: -f1 | sort -u)

for file in $files; do
    echo "Fixing $file"
    # This is a simplified fix - review each change manually
    sed -i.bak 's/return err$/return fmt.Errorf("operation failed: %w", err)/g' "$file"
done

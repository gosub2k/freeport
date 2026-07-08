#!/bin/bash

# Usage: ./cleanup-pod-storage.sh [pod-name] [namespace]

POD_NAME=$1
NAMESPACE=${2:-default} # Defaults to 'default' if not provided

# 1. Interactive Pod Selection if not provided
if [ -z "$POD_NAME" ]; then
    echo "🔍 No pod name provided. Select a pod in namespace '$NAMESPACE'..."
    POD_NAME=$(kubectl get pods -n "$NAMESPACE" --no-headers -o custom-columns=":metadata.name" | fzf --header="Select Pod to Delete (Namespace: $NAMESPACE)")
    
    if [ -z "$POD_NAME" ]; then
        echo "❌ No pod selected. Exiting."
        exit 1
    fi
fi

echo "🎯 Target: Pod '$POD_NAME' in Namespace '$NAMESPACE'"

# 2. Get PVC names used by the pod
PVC_NAMES=$(kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.volumes[*].persistentVolumeClaim.claimName}' 2>/dev/null)

if [ -z "$PVC_NAMES" ]; then
    echo "⚠️  No PVCs found for pod '$POD_NAME'. Deleting pod only."
    kubectl delete pod "$POD_NAME" -n "$NAMESPACE" --ignore-not-found=true
    exit 0
fi

echo "📦 Found PVCs: $PVC_NAMES"

# 3. Force delete the Pod first
echo "🗑️  Force deleting pod '$POD_NAME'..."
kubectl delete pod "$POD_NAME" -n "$NAMESPACE" --grace-period=0 --force --ignore-not-found=true

# 4. Process each PVC
for PVC_NAME in $PVC_NAMES; do
    echo ""
    echo "🔄 Processing PVC: $PVC_NAME"
    
    # Get the bound PV name
    PV_NAME=$(kubectl get pvc "$PVC_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.volumeName}' 2>/dev/null)
    
    if [ -n "$PV_NAME" ]; then
        echo "   ↳ Bound to PV: $PV_NAME"
        
        # Remove PV finalizers
        echo "   ↳ Removing PV finalizers..."
        kubectl patch pv "$PV_NAME" -p '{"metadata":{"finalizers":null}}' --type=merge 2>/dev/null
        
        # Delete PV
        echo "   ↳ Deleting PV..."
        kubectl delete pv "$PV_NAME" --grace-period=0 --force --ignore-not-found=true
    fi
    
    # Remove PVC finalizers
    echo "   ↳ Removing PVC finalizers..."
    kubectl patch pvc "$PVC_NAME" -n "$NAMESPACE" -p '{"metadata":{"finalizers":null}}' --type=merge 2>/dev/null
    
    # Delete PVC
    echo "   ↳ Deleting PVC..."
    kubectl delete pvc "$PVC_NAME" -n "$NAMESPACE" --grace-period=0 --force --ignore-not-found=true
done

echo ""
echo "✅ Cleanup complete for pod '$POD_NAME'."   

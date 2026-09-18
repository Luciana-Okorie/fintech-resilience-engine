$API = "http://localhost:8090"
$ProviderA = "http://localhost:9001"

Write-Host "=== 1. Baseline ===" -ForegroundColor Cyan
Invoke-RestMethod -Uri "$ProviderA/mode" -Method Post -Body '{"mode":"NORMAL"}' -ContentType "application/json"
Start-Sleep -Seconds 6
Invoke-RestMethod "$API/health" | ConvertTo-Json -Depth 5

Write-Host "=== 2. Fail provider-a ===" -ForegroundColor Cyan
Invoke-RestMethod -Uri "$ProviderA/mode" -Method Post -Body '{"mode":"DOWN"}' -ContentType "application/json"
Write-Host "Waiting for detection + breaker trip..."
Start-Sleep -Seconds 130
Invoke-RestMethod "$API/health" | ConvertTo-Json -Depth 5
Invoke-RestMethod "$API/breakers" | ConvertTo-Json -Depth 5
Invoke-RestMethod "$API/graph/impact?service=provider-a" | ConvertTo-Json -Depth 5
Invoke-RestMethod "$API/incidents" | ConvertTo-Json -Depth 5

Write-Host "=== 3. Simulate repeated calls (should trip the breaker) ===" -ForegroundColor Cyan

for ($i = 1; $i -le 10; $i++) {
    $result = Invoke-RestMethod -Uri "$API/simulate/call" -Method Post `
      -Body '{"dependency":"provider-a","idempotency_key":"demo-key-001"}' `
      -ContentType "application/json"

    Write-Host "Call $i -> success=$($result.success) used_fallback=$($result.used_fallback) reason=$($result.reason)"
    Start-Sleep -Milliseconds 500
}

Invoke-RestMethod "$API/breakers" | ConvertTo-Json -Depth 5
Invoke-RestMethod "$API/incidents" | ConvertTo-Json -Depth 5
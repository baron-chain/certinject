param (
  $physical_store
)

$logical_store = "Disallowed"

Write-Host "----- Running TLS handshake tests for $physical_store/$logical_store -----"

Write-Host "----- Publicly trusted TLS website; no injection -----"

& "powershell" "-ExecutionPolicy" "Unrestricted" "-File" "testdata/try-tls-handshake.ps1" "-url" "https://www.namecoin.org/"
If (!$?) {
  exit 222
}

# Extract via: torsocks openssl s_client -showcerts -servername www.namecoin.org -connect www.namecoin.org:443 < /dev/null | csplit -f tmp-namecoin.org- - '/-----BEGIN CERTIFICATE-----/' '{*}' && grep -v : tmp-namecoin.org-02 > testdata/lets-encrypt-intermediate.ca.pem.cert && rm tmp-namecoin.org-*
Write-Host "----- Publicly trusted TLS website; injecting intermediate CA PEM certificate into $physical_store/$logical_store -----"
# inject certificate into trust store
Write-Host "injecting certificate into trust store"
& "certinject.exe" "-capi.physical-store" "$physical_store" "-capi.logical-store" "$logical_store" "-certinject.cert" "testdata/lets-encrypt-intermediate.ca.pem.cert" "-certstore.cryptoapi"
If (!$?) {
  Write-Host "certificate injection failed"
  exit 222
}

Write-Host "Waiting for cache to clear"
Start-Sleep -seconds 30

& "powershell" "-ExecutionPolicy" "Unrestricted" "-File" "testdata/try-tls-handshake.ps1" "-url" "https://www.namecoin.org/" "-fail"
If (!$?) {
  exit 222
}

Write-Host "----- Cleanup $physical_store/$logical_store via certutil -----"
$root_cn = "R3"
If ( "system" -eq $physical_store ) {
  & "certutil" "-delstore" "$logical_store" "$root_cn"
  If (!$?) {
    exit 222
  }
}
If ( "enterprise" -eq $physical_store ) {
  & "certutil" "-enterprise" "-delstore" "$logical_store" "$root_cn"
  If (!$?) {
    exit 222
  }
}
If ( "group-policy" -eq $physical_store ) {
  & "certutil" "-grouppolicy" "-delstore" "$logical_store" "$root_cn"
  If (!$?) {
    exit 222
  }
}

# all done
Write-Host "----- All TLS handshake tests for $physical_store/$logical_store passed -----"
exit 0

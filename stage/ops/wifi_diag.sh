#!/system/bin/sh
echo "=== wpa status ==="
wpa_cli -i wlan0 status 2>/dev/null | head -n 20
echo "=== ip wlan0 ==="
ip addr show wlan0 2>/dev/null | head -n 12
echo "=== dumpsys wifi (state) ==="
dumpsys wifi 2>/dev/null | grep -iE 'Wi-Fi is|mNetworkInfo|SSID|Supplicant state|Detailed state|curState' | head -n 25
echo "=== saved networks ==="
wpa_cli -i wlan0 list_networks 2>/dev/null | head -n 20
echo "=== wifi store ssids ==="
grep -o '<string name="SSID">[^<]*</string>' /data/misc/wifi/WifiConfigStore.xml 2>/dev/null | head -n 20
echo "=== logcat wifi tail ==="
logcat -d -t 2000 2>/dev/null | grep -iE 'wpa_supplicant|DhcpClient|WifiClientModeImpl|WifiStateMachine|wlan0' | tail -n 60

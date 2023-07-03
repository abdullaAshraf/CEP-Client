# CEP Client

Community-oriented Edge computing Platform (CEP) client

This is the client-side code for my Master's degree thesis project available at https://qspace.library.queensu.ca/bitstream/handle/1974/30378/Moustafa_Abdalla_A_202209_MASc.pdf.

This repository covers the main logic for the client and cluster head described in section 3.3.1. At least one device must be registered per cluster and it needs to act as the cluster head. Any other devices can be started in normal mode where they can be assigned work and request services of their own from other devices in the same cluster or any communities accessible by the cluster owner.

## How To Deploy
To start this client application you need a supported platform like a Linux machine, check the link for a complete list of platforms: https://github.com/lf-edge/edge-home-orchestration-go#platforms-supported.

* After hosting the server-side application available at https://github.com/abdullaAshraf/CEP-Server update the ```{serverUrl}``` under ```internal/controller/discoverymgr/discovery.go``` and ```internal/restinterface/client/restclient/restclient.go``` references in the code with the server public URL. Then register a new user using the Authentication module provided in the server and request an access token then update the code reference to this at ```{serverAuthKey}``` under ```internal/restinterface/resthelper/helper.go```.

* Build the go application in docker mode run:
```
make distclean
make create_context CONFIGFILE=x86_64c
make
```

* Run cluster head:
```
/deployments/datastorage/docker-compose up -d
make run
```

* Run other devices:
```
./mitmproxy 
make run
```
Note: mitmproxy is used as a workaround to redirect all service queries by other devices to the cluster head by responding with a zero score to any REST query about benchmarks originating from a machine different than the cluster head.

* To view application logs:
```
docker logs -f edge-orchestration
```

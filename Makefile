DELAY = 100ms
name = simple

topo:
	make -C ./mininet 
ctrl:
	make -C ./controller
attack:
	make -C ./lucid attack
lucid:
	make -C ./lucid lucid


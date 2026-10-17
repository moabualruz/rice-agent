query groups (1):
Q1 [primary]: bake sourdough bread loaf
hits: 5

#1 matchedBy=fts+vector score=0.0325 wanted/node-environment-shim.md:1-7
heading: node-environment-shim
1	# node-environment-shim
trace: query "bake sourdough bread loaf": fts #1, vector #1; fused #1

#2 matchedBy=fts+vector score=0.0320 tool/gone.md:1-7
trace: query "bake sourdough bread loaf": fts #2, vector #2; fused #2

#3 matchedBy=fts+vector score=0.0318 skill/release-judgment.md:1-7
trace: query "bake sourdough bread loaf": fts #5, vector #2; fused #3

#4 matchedBy=fts score=0.0161 tool/pr-flow.md:1-7
trace: query "bake sourdough bread loaf": fts #3; fused #4

#5 matchedBy=vector score=0.0159 provider/token-meter.md:1-6
trace: query "bake sourdough bread loaf": vector #4; fused #5

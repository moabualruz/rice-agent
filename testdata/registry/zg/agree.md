query groups (1):
Q1 [primary]: bake sourdough bread loaf
hits: 3

#1 matchedBy=fts+vector score=0.0328 tool/host-run.md:1-7
heading: host-run
heading_level: 1
1	# host-run
trace: query "bake sourdough bread loaf": fts #1, vector #1; fused #1

#2 matchedBy=fts+vector score=0.0313 provider/dupe-finder.md:1-8
heading: dupe-finder
heading_level: 1
1	# dupe-finder
trace: query "bake sourdough bread loaf": fts #2, vector #3; fused #2

#3 matchedBy=vector score=0.0161 tool/pr-flow.md:1-7
heading: pr-flow
heading_level: 1
1	# pr-flow
trace: query "bake sourdough bread loaf": vector #2; fused #3

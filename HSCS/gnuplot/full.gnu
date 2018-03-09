#!/usr/bin/env gnuplot

set term postscript eps enhanced color font 'Helvetica,22' linewidth 2
set output '../figure/full.eps'
set style data histograms

set xtics border in scale 0,0 nomirror rotate by -45 autojustify
set style fill solid 1.00 border lt -1

set key font ",20" width 3

set xlabel "Workloads (threads, resolution)"
set ylabel "FPS"

plot '../data/performance.dat' using 2:xtic(1) ti col, \
	'' u 3 ti col, \
	'' u 4 ti col

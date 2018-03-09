#!/usr/bin/env gnuplot

set term postscript eps enhanced color font 'Helvetica,22' linewidth 2
set output '../figure/vanilla.eps'
set style data histograms

set xtics border in scale 0,0 nomirror rotate by -45 autojustify
set style fill solid 1.00 border lt -1

set xlabel "Workloads (threads, resolution)"
set ylabel "FPS"

plot '../data/performance_full.dat' using 2:xtic(1) ti col, \
	'' u 3 ti col

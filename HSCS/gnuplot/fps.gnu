#!/usr/bin/env gnuplot

set term postscript eps enhanced color font 'Helvetica,20' linewidth 2
set output '../figure/fps.eps'
set style data linespoints
set style line 1  linecolor rgb "red"  linewidth 3.000 pointtype 2 dashtype 2 pointsize default pointinterval 0
set style line 2  linecolor rgb "orange"  linewidth 2.000 pointtype 2 dashtype 2 pointsize default pointinterval 0
set style line 3  linecolor rgb "yellow"  linewidth 3.000 pointtype 2 dashtype 2 pointsize default pointinterval 0
set style line 4  linecolor rgb "green"  linewidth 2.000 pointtype 2 dashtype 2 pointsize default pointinterval 0
set datafile missing "-"

set xtics border in scale 0,0 nomirror rotate by -45 autojustify
#set style fill pattern

set xlabel "Scenarios"
set ylabel "FPS "

plot '../data/fps.dat' using 2:xtic(1) lt -1 pi -4 pt 6 ti col, \
                    '' u 3             lt -1 pi -3 pt 7 ti col, \
                    '' u 4             lt -1 pi -6 pt 7 ti col, \
                    '' u 5             lt -1 pi -3 pt 4 ti col, \
                    '' u 6             lt -1 pi -5 pt 5 ti col, \
                    '' u 7             lt -1 ti col
